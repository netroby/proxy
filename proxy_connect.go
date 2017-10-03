package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/getlantern/errors"
	"github.com/getlantern/lampshade"
	"github.com/getlantern/netx"
	"github.com/getlantern/proxy/filters"
	"github.com/getlantern/reconn"
)

const (
	connectRequest = "CONNECT %v HTTP/1.1\r\nHost: %v\r\n\r\n"

	maxHTTPSize = 2 << 15 // 65K
)

// BufferSource is a source for buffers used in reading/writing.
type BufferSource interface {
	Get() []byte
	Put(buf []byte)
}

func (opts *Opts) applyCONNECTDefaults() {
	// Apply defaults
	if opts.BufferSource == nil {
		opts.BufferSource = &defaultBufferSource{}
	}
}

// interceptor configures an Interceptor.
type connectInterceptor struct {
	idleTimeout        time.Duration
	bufferSource       BufferSource
	dial               DialFunc
	okWaitsForUpstream bool
}

func (proxy *proxy) nextCONNECT(downstream net.Conn) filters.Next {
	return func(ctx filters.Context, modifiedReq *http.Request) (*http.Response, filters.Context, error) {
		suppressOK := ctx.Value(ctxKeyNoRespondOkay) != nil

		var resp *http.Response
		nextCtx := ctx.WithValue(ctxKeyUpstreamAddr, modifiedReq.URL.Host)
		respondOK := func() {
			if !suppressOK {
				resp, nextCtx, _ = filters.ShortCircuit(nextCtx, modifiedReq, &http.Response{
					StatusCode: http.StatusOK,
				})
			}
		}

		if !proxy.OKWaitsForUpstream {
			// We preemptively respond with an OK on the client. Some user agents like
			// Chrome consider any non-200 OK response from the proxy to indicate that
			// there's a problem with the proxy rather than the origin, causing the user
			// agent to mark the proxy itself as bad and avoid using it in the future.
			// By immediately responding 200 OK irrespective of what happens with the
			// origin, we are signaling to the user agent that the proxy itself is good.
			// If there is a subsequent problem dialing the origin, the user agent will
			// (mostly correctly) attribute that to a problem with the origin rather
			// than the proxy and continue to consider the proxy good. See the extensive
			// discussion here: https://github.com/getlantern/lantern/issues/5514.
			respondOK()
			return resp, nextCtx, nil
		}

		// Note - for CONNECT requests, we use the Host from the request URL, not the
		// Host header. See discussion here:
		// https://ask.wireshark.org/questions/22988/http-host-header-with-and-without-port-number
		upstream, err := proxy.Dial(ctx, true, "tcp", modifiedReq.URL.Host)
		if err != nil {
			if proxy.OKWaitsForUpstream {
				return badGateway(ctx, modifiedReq, err)
			}
			log.Error(err)
			return nil, ctx, err
		}

		// In this case, waited to successfully dial upstream before responding
		// OK. Lantern uses this logic on server-side proxies so that the Lantern
		// client retains the opportunity to fail over to a different proxy server
		// just in case that one is able to reach the origin. This is relevant,
		// for example, if some proxy servers reside in jurisdictions where an
		// origin site is blocked but other proxy servers don't.
		respondOK()
		nextCtx = nextCtx.WithValue(ctxKeyUpstream, upstream)
		return resp, nextCtx, nil
	}
}

func (proxy *proxy) Connect(ctx context.Context, in io.Reader, conn net.Conn, origin string) error {
	pin := io.MultiReader(strings.NewReader(fmt.Sprintf(connectRequest, origin, origin)), in)
	return proxy.Handle(context.WithValue(ctx, ctxKeyNoRespondOkay, "true"), pin, conn)
}

func (proxy *proxy) proceedWithConnect(ctx filters.Context, upstreamAddr string, upstream net.Conn, downstream net.Conn) error {
	if upstream == nil {
		var dialErr error
		upstream, dialErr = proxy.Dial(ctx, true, "tcp", upstreamAddr)
		if dialErr != nil {
			return dialErr
		}
	}
	defer func() {
		if closeErr := upstream.Close(); closeErr != nil {
			log.Tracef("Error closing upstream connection: %s", closeErr)
		}
	}()

	var rr io.Reader
	if proxy.mitmIC != nil {
		// Try to MITM the connection
		downstreamMITM, upstreamMITM, mitming, err := proxy.mitmIC.MITM(downstream, upstream)
		if err != nil {
			log.Errorf("Unable to MITM %v: %v", upstreamAddr, err)
			return errors.New("Unable to MITM connection: %v", err)
		}
		downstream = downstreamMITM
		upstream = upstreamMITM
		if mitming {
			handshakeErr := upstream.(*tls.Conn).Handshake()
			if handshakeErr != nil {
				log.Errorf("HandshakeErr: %v", handshakeErr)
				return handshakeErr
			}
		}

		if mitming {
			// Try to read HTTP request and process as HTTP assuming that requests
			// (not including body) are always smaller than 65K. If this assumption is
			// violated, we won't be able to process the data on this connection.
			downstreamRR := reconn.Wrap(downstream, maxHTTPSize)
			downstreamRRBuf := bufio.NewReadWriter(bufio.NewReader(downstreamRR), bufio.NewWriter(downstreamRR))
			_, peekReqErr := http.ReadRequest(downstreamRRBuf.Reader)
			var rrErr error
			rr, rrErr = downstreamRR.Rereader()
			if rrErr != nil {
				// Reading request overflowed, abort
				return errors.New("Unable to re-read data: %v", rrErr)
			}
			if peekReqErr == nil {
				// Handle as HTTP, prepend already read HTTP request
				fullDownstream := io.MultiReader(rr, downstream)
				// Remove upstream info from context so that handle doesn't try to
				// process this as a CONNECT
				ctx = ctx.WithValue(ctxKeyUpstream, nil).WithValue(ctxKeyUpstreamAddr, nil)
				return proxy.handle(ctx, fullDownstream, downstream, upstream)
			}

			// We couldn't read the first HTTP Request, fall back to piping data
		}
	}

	// Prepare to pipe data between the client and the proxy.
	bufOut := proxy.BufferSource.Get()
	bufIn := proxy.BufferSource.Get()
	defer proxy.BufferSource.Put(bufOut)
	defer proxy.BufferSource.Put(bufIn)

	if rr != nil {
		// We tried and failed to MITM. First copy already read data to upstream
		// before we start piping as usual
		_, copyErr := io.CopyBuffer(upstream, rr, bufOut)
		if copyErr != nil {
			return errors.New("Error copying initial data to upstream: %v", copyErr)
		}
	}

	// Pipe data between the client and the proxy.
	writeErr, readErr := netx.BidiCopy(upstream, downstream, bufOut, bufIn)
	if isUnexpected(readErr) {
		return errors.New("Error piping data to downstream: %v", readErr)
	} else if isUnexpected(writeErr) {
		return errors.New("Error piping data to upstream: %v", writeErr)
	}
	return nil
}

func badGateway(ctx filters.Context, req *http.Request, err error) (*http.Response, filters.Context, error) {
	log.Debugf("Responding BadGateway: %v", err)
	return filters.Fail(ctx, req, http.StatusBadGateway, err)
}

type defaultBufferSource struct{}

func (dbs *defaultBufferSource) Get() []byte {
	// We limit ourselves to lampshade.MaxDataLen to ensure compatibility with it
	return make([]byte, lampshade.MaxDataLen)
}

func (dbs *defaultBufferSource) Put(buf []byte) {
	// do nothing
}
