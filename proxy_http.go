package proxy

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/getlantern/errors"
	"github.com/getlantern/netx"
	"github.com/getlantern/preconn"
	"github.com/getlantern/proxy/filters"
)

func (opts *Opts) applyHTTPDefaults() {
	// Apply defaults
	if opts.Filter == nil {
		opts.Filter = filters.FilterFunc(defaultFilter)
	}
	if opts.OnError == nil {
		opts.OnError = defaultOnError
	}
	if opts.IdleTimeout > 0 {
		opts.Filter = filters.Join(filters.FilterFunc(func(ctx filters.Context, req *http.Request, next filters.Next) (*http.Response, filters.Context, error) {
			return next(ctx, req)
		}), opts.Filter)
	}
}

// Handle implements the interface Proxy
func (proxy *proxy) Handle(ctx context.Context, downstreamIn io.Reader, downstream net.Conn) (err error) {
	defer func() {
		p := recover()
		if p != nil {
			safeClose(downstream)
			err = errors.New("Recovered from panic handling connection: %v", p)
		}
	}()
	var buffer bytes.Buffer
	err = proxy.handle(context.WithValue(ctx, "buffer", &buffer), downstreamIn, downstream, nil)
	if buffer.Len() > 0 && buffer.Len() < 300 {
		log.Debugf("Response wrote on the wire:\n***\n%s\n***", buffer.String())
	}

	return
}

func safeClose(conn net.Conn) {
	defer func() {
		p := recover()
		if p != nil {
			log.Errorf("Panic on closing connection: %v", p)
		}
	}()

	conn.Close()
}

func (proxy *proxy) handle(ctx context.Context, downstreamIn io.Reader, downstream net.Conn, upstream net.Conn) error {
	defer func() {
		if closeErr := downstream.Close(); closeErr != nil {
			log.Tracef("Error closing downstream connection: %s", closeErr)
		}
	}()

	downstreamBuffered := bufio.NewReader(downstreamIn)
	fctx := filters.WrapContext(withAwareConn(ctx), downstream)

	// Read initial request
	req, err := http.ReadRequest(downstreamBuffered)
	if req != nil {
		remoteAddr := downstream.RemoteAddr()
		if remoteAddr != nil {
			req.RemoteAddr = downstream.RemoteAddr().String()
		}
		if origURLScheme(ctx) == "" {
			fctx = fctx.
				WithValue(ctxKeyOrigURLScheme, req.URL.Scheme).
				WithValue(ctxKeyOrigURLHost, req.URL.Host).
				WithValue(ctxKeyOrigHost, req.Host)
		}
	}
	if err != nil {
		if isUnexpected(err) {
			errResp := proxy.OnError(fctx, req, true, err)
			if errResp != nil {
				proxy.writeResponse(downstream, req, errResp)
			}
			return errors.New("Error in initial ReadRequest: %v", err)
		}
		return nil
	}

	var next filters.Next
	if req.Method == http.MethodConnect {
		next = proxy.nextCONNECT(downstream)
	} else {
		var tr *http.Transport
		if upstream != nil {
			setUpstreamForAwareConn(fctx, upstream)
			tr = &http.Transport{
				DialContext: func(ctx context.Context, net, addr string) (net.Conn, error) {
					// always use the supplied upstream connection, but don't allow it to
					// be closed by the transport
					return &noCloseConn{upstream}, nil
				},
				// this transport is only used once, don't keep any idle connections,
				// however still allow the transport to close the connection after using
				// it
				MaxIdleConnsPerHost: -1,
			}
		} else {
			tr = &http.Transport{
				DialContext: func(ctx context.Context, net, addr string) (net.Conn, error) {
					conn, err := proxy.Dial(ctx, false, net, addr)
					if err == nil {
						// On first dialing conn, handle RequestAware
						setUpstreamForAwareConn(ctx, conn)
						handleRequestAware(ctx)
					}
					return conn, err
				},
				IdleConnTimeout: proxy.IdleTimeout,
				// since we have one transport per downstream connection, we don't need
				// more than this
				MaxIdleConnsPerHost: 1,
			}
		}

		defer tr.CloseIdleConnections()
		next = func(ctx filters.Context, modifiedReq *http.Request) (*http.Response, filters.Context, error) {
			modifiedReq = modifiedReq.WithContext(ctx)
			setRequestForAwareConn(ctx, modifiedReq)
			handleRequestAware(ctx)
			resp, err := tr.RoundTrip(prepareRequest(modifiedReq))
			handleResponseAware(ctx, modifiedReq, resp, err)
			return resp, ctx, err
		}
	}

	return proxy.processRequests(fctx, req.RemoteAddr, req, downstream, downstreamBuffered, next)
}

func (proxy *proxy) processRequests(ctx filters.Context, remoteAddr string, req *http.Request, downstream net.Conn, downstreamBuffered *bufio.Reader, next filters.Next) error {
	var readErr error
	var resp *http.Response
	var err error

	for {
		if req.URL.Scheme == "" {
			req.URL.Scheme = origURLScheme(ctx)
		}
		if req.URL.Host == "" {
			req.URL.Host = origURLHost(ctx)
		}
		if req.Host == "" {
			req.Host = origHost(ctx)
		}
		resp, ctx, err = proxy.Filter.Apply(ctx, req, next)
		if err != nil && resp == nil {
			resp = proxy.OnError(ctx, req, false, err)
		}

		if resp != nil {
			buffer := ctx.Value("buffer").(*bytes.Buffer)
			ds := io.MultiWriter(downstream, buffer)
			writeErr := proxy.writeResponse(ds, req, resp)
			if writeErr != nil {
				if isUnexpected(writeErr) {
					return errors.New("Unable to write response to downstream: %v", writeErr)
				}
				// Error is not unexpected, but we're done
				return err
			}
		}

		upstream := upstreamConn(ctx)
		upstreamAddr := upstreamAddr(ctx)
		isConnect := upstream != nil || upstreamAddr != ""

		buffered := downstreamBuffered.Buffered()
		if buffered > 0 {
			b, _ := downstreamBuffered.Peek(buffered)
			downstream = preconn.Wrap(downstream, b)
		}

		if isConnect {
			return proxy.proceedWithConnect(ctx, req, upstreamAddr, upstream, downstream)
		}

		if req.Close {
			// Client signaled that they would close the connection after this
			// request, finish
			return err
		}

		if err == nil && resp != nil && resp.Close {
			// Last response, finish
			return err
		}

		// read the next request
		req, readErr = http.ReadRequest(downstreamBuffered)
		if readErr != nil {
			if isUnexpected(readErr) {
				errResp := proxy.OnError(ctx, req, true, readErr)
				if errResp != nil {
					proxy.writeResponse(downstream, req, errResp)
				}
				return errors.New("Unable to read next request from downstream: %v", readErr)
			}
			return err
		}

		// Preserve remote address from original request
		ctx = ctx.IncrementRequestNumber()
		req.RemoteAddr = remoteAddr
		req = req.WithContext(ctx)
	}
}

func handleRequestAware(ctx context.Context) {
	upstream := upstreamForAwareConn(ctx)
	if upstream == nil {
		return
	}

	netx.WalkWrapped(upstream, func(wrapped net.Conn) bool {
		switch t := wrapped.(type) {
		case RequestAware:
			req := requestForAwareConn(ctx)
			t.OnRequest(req)
		}
		return true
	})
}

func handleResponseAware(ctx context.Context, req *http.Request, resp *http.Response, err error) {
	upstream := upstreamForAwareConn(ctx)
	if upstream == nil {
		return
	}

	netx.WalkWrapped(upstream, func(wrapped net.Conn) bool {
		switch t := wrapped.(type) {
		case ResponseAware:
			t.OnResponse(req, resp, err)
		}
		return true
	})
}

func (proxy *proxy) writeResponse(downstream io.Writer, req *http.Request, resp *http.Response) error {
	if resp.Request == nil {
		resp.Request = req
	}
	out := downstream
	if resp.ProtoMajor == 0 {
		resp.ProtoMajor = 1
		resp.ProtoMinor = 1
	}
	belowHTTP11 := !resp.ProtoAtLeast(1, 1)
	if belowHTTP11 && resp.StatusCode < 200 {
		// HTTP 1.0 doesn't define status codes below 200, discard response
		// see http://coad.measurement-factory.com/cgi-bin/coad/SpecCgi?spec_id=rfc2616#excerpt/rfc2616/859a092cb26bde76c25284196171c94d
		out = ioutil.Discard
	} else {
		resp = prepareResponse(resp, belowHTTP11)
		proxy.addIdleKeepAlive(resp.Header)
	}
	var buffer bytes.Buffer
	mw := io.MultiWriter(out, &buffer)
	err := resp.Write(mw)
	// resp.Write closes the body only if it's successfully sent. Close
	// manually when error happens.
	if err != nil && resp.Body != nil {
		resp.Body.Close()
	}
	log.Debugf("Wrote response:\n%v", buffer.String())
	return err
}

// prepareRequest prepares the request in line with the HTTP spec for proxies.
func prepareRequest(req *http.Request) *http.Request {
	req.Proto = "HTTP/1.1"
	req.ProtoMajor = 1
	req.ProtoMinor = 1
	// Overwrite close flag: keep persistent connection for the backend servers
	req.Close = false

	// Request Header
	newHeader := make(http.Header)
	copyHeadersForForwarding(newHeader, req.Header)
	// Ensure we have a HOST header (important for Go 1.6+ because http.Server
	// strips the HOST header from the inbound request)
	newHeader.Set("Host", req.Host)
	req.Header = newHeader

	// Request URL
	req.URL = cloneURL(req.URL)
	// If req.URL.Scheme was blank, it's http. Otherwise, it's https and we leave
	// it alone.
	if req.URL.Scheme == "" {
		req.URL.Scheme = "http"
	}
	// We need to make sure the host is defined in the URL (not the actual URI)
	req.URL.Host = req.Host

	userAgent := req.UserAgent()
	if userAgent == "" {
		req.Header.Del("User-Agent")
	} else {
		req.Header.Set("User-Agent", userAgent)
	}

	return req
}

// prepareResponse prepares the response in line with the HTTP spec
func prepareResponse(resp *http.Response, belowHTTP11 bool) *http.Response {
	origHeader := resp.Header
	resp.Header = make(http.Header)
	copyHeadersForForwarding(resp.Header, origHeader)
	// Below added due to CoAdvisor test failure
	if resp.Header.Get("Date") == "" {
		resp.Header.Set("Date", time.Now().Format(time.RFC850))
	}
	if belowHTTP11 {
		// Also, make sure we're not sending chunked transfer encoding to 1.0 clients
		resp.TransferEncoding = nil
	}
	return resp
}

// cloneURL provides update safe copy by avoiding shallow copying User field
func cloneURL(i *url.URL) *url.URL {
	out := *i
	if i.User != nil {
		out.User = &(*i.User)
	}
	return &out
}

// copyHeadersForForwarding will copy the headers but filter those that shouldn't be
// forwarded
func copyHeadersForForwarding(dst, src http.Header) {
	var extraHopByHopHeaders []string
	for k, vv := range src {
		switch k {
		// Skip hop-by-hop headers, ref section 13.5.1 of http://www.ietf.org/rfc/rfc2616.txt
		case "Connection":
			// section 14.10 of rfc2616
			// the slice is short typically, don't bother sort it to speed up lookup
			extraHopByHopHeaders = vv
		case "Keep-Alive":
		case "Proxy-Authenticate":
		case "Proxy-Authorization":
		case "TE":
		case "Trailers":
		case "Transfer-Encoding":
		case "Upgrade":
		default:
			if !contains(k, extraHopByHopHeaders) {
				for _, v := range vv {
					dst.Add(k, v)
				}
			}
		}
	}
}

func contains(k string, s []string) bool {
	for _, h := range s {
		if k == h {
			return true
		}
	}
	return false
}

func isUnexpected(err error) bool {
	if err == nil {
		return false
	}
	if err == io.EOF {
		return false
	}
	// This is okay per the HTTP spec.
	// See https://www.w3.org/Protocols/rfc2616/rfc2616-sec8.html#sec8.1.4
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return false
	}

	text := err.Error()
	return !strings.HasSuffix(text, "EOF") &&
		!strings.Contains(text, "i/o timeout") &&
		!strings.Contains(text, "Use of idled network connection") &&
		!strings.Contains(text, "use of closed network connection") &&
		// usually caused by client disconnecting
		!strings.Contains(text, "broken pipe") &&
		// usually caused by client disconnecting
		!strings.Contains(text, "connection reset by peer")
}

func defaultFilter(ctx filters.Context, req *http.Request, next filters.Next) (*http.Response, filters.Context, error) {
	return next(ctx, req)
}

func defaultOnError(ctx filters.Context, req *http.Request, read bool, err error) *http.Response {
	return nil
}
