package proxy

import (
	"bytes"
	"context"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/getlantern/errors"
	"github.com/getlantern/hidden"
	"github.com/getlantern/idletiming"
	"github.com/getlantern/netx"
)

// BufferSource is a source for buffers used in reading/writing.
type BufferSource interface {
	Get() []byte
	Put(buf []byte)
}

// CONNECT returns a Handler that processes CONNECT requests.
//
// idleTimeout: if specified, lets us know to include an appropriate
// KeepAlive: timeout header in the CONNECT response
//
// bufferSource: specifies a BufferSource, leave nil to use default
//
// okWaitsForUpstream: specifies whether or not to wait on dialing upstream
// before responding OK
//
// dial: the function that's used to dial upstream
func CONNECT(
	idleTimeout time.Duration,
	bufferSource BufferSource,
	okWaitsForUpstream bool,
	dial DialFunc,
) Interceptor {
	// Apply defaults
	if bufferSource == nil {
		bufferSource = &defaultBufferSource{}
	}

	ic := &connectInterceptor{
		idleTimeout:        idleTimeout,
		bufferSource:       bufferSource,
		dial:               dial,
		okWaitsForUpstream: okWaitsForUpstream,
	}
	return ic.connect
}

// interceptor configures an Interceptor.
type connectInterceptor struct {
	idleTimeout        time.Duration
	bufferSource       BufferSource
	dial               DialFunc
	okWaitsForUpstream bool
}

func (ic *connectInterceptor) connect(ctx context.Context, w http.ResponseWriter, req *http.Request) error {
	var downstream net.Conn
	var upstream net.Conn
	var err error

	closeDownstream := false
	closeUpstream := false
	defer func() {
		if closeDownstream {
			if closeErr := downstream.Close(); closeErr != nil {
				log.Tracef("Error closing downstream connection: %s", closeErr)
			}
		}
		if closeUpstream {
			if closeErr := upstream.Close(); closeErr != nil {
				log.Tracef("Error closing upstream connection: %s", closeErr)
			}
		}
	}()

	if downstream, err = ic.hijack(w); err != nil {
		return err
	}
	closeDownstream = true

	if !ic.okWaitsForUpstream {
		err = ic.respondOK(downstream, req, w.Header())
		if err != nil {
			return err
		}
	}

	// Note - for CONNECT requests, we use the Host from the request URL, not the
	// Host header. See discussion here:
	// https://ask.wireshark.org/questions/22988/http-host-header-with-and-without-port-number
	if upstream, err = ic.dial(ctx, "tcp", req.URL.Host); err != nil {
		if ic.okWaitsForUpstream {
			respondBadGatewayHijacked(downstream, req, w.Header(), err)
		} else {
			log.Error(err)
		}
		return err
	}
	closeUpstream = true

	if ic.okWaitsForUpstream {
		err = ic.respondOK(downstream, req, w.Header())
		if err != nil {
			return err
		}
	}

	return ic.copy(upstream, downstream)
}

func (ic *connectInterceptor) copy(upstream, downstream net.Conn) error {
	// Pipe data between the client and the proxy.
	bufOut := ic.bufferSource.Get()
	bufIn := ic.bufferSource.Get()
	defer ic.bufferSource.Put(bufOut)
	defer ic.bufferSource.Put(bufIn)
	writeErr, readErr := netx.BidiCopy(upstream, downstream, bufOut, bufIn)
	// Note - we ignore idled errors because these are okay per the HTTP spec.
	// See https://www.w3.org/Protocols/rfc2616/rfc2616-sec8.html#sec8.1.4
	// We also ignore "broken pipe" errors on piping to downstream because they're
	// usually caused by the client disconnecting and we don't worry about that.
	if readErr != nil && readErr != io.EOF && !strings.Contains(readErr.Error(), "broken pipe") {
		return errors.New("Error piping data to downstream: %v", readErr)
	} else if writeErr != nil && writeErr != idletiming.ErrIdled {
		return errors.New("Error piping data to upstream: %v", writeErr)
	}
	return nil
}

func (ic *connectInterceptor) hijack(w http.ResponseWriter) (net.Conn, error) {
	// Hijack underlying connection.
	downstream, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		// If there's an error hijacking, it's because the connection has already
		// been hijacked (a programming error). Not much we can do other than
		// return an error.
		fullErr := errors.New("Unable to hijack connection: %s", err)
		log.Error(fullErr)
		return nil, fullErr
	}
	return downstream, nil
}

func (ic *connectInterceptor) respondOK(writer io.Writer, req *http.Request, respHeaders http.Header) error {
	addIdleKeepAlive(respHeaders, ic.idleTimeout)
	err := respondHijacked(writer, req, http.StatusOK, respHeaders, nil)
	if err != nil {
		fullErr := errors.New("Unable to respond OK: ", err)
		log.Error(fullErr)
		return fullErr
	}
	return nil
}

func respondBadGatewayHijacked(writer io.Writer, req *http.Request, respHeaders http.Header, err error) {
	log.Debugf("Responding BadGateway: %v", err)
	respondHijacked(writer, req, http.StatusBadGateway, respHeaders, []byte(hidden.Clean(err.Error())))
}

func respondHijacked(writer io.Writer, req *http.Request, statusCode int, respHeaders http.Header, body []byte) error {
	defer func() {
		if req.Body != nil {
			if err := req.Body.Close(); err != nil {
				log.Debugf("Error closing body of request: %s", err)
			}
		}
	}()

	if respHeaders == nil {
		respHeaders = make(http.Header)
	}
	resp := &http.Response{
		Header:     respHeaders,
		StatusCode: statusCode,
		ProtoMajor: 1,
		ProtoMinor: 1,
	}
	if body != nil {
		resp.Body = ioutil.NopCloser(bytes.NewReader(body))
	}
	return resp.Write(writer)
}

type defaultBufferSource struct{}

func (dbs *defaultBufferSource) Get() []byte {
	// We use the same default buffer size as io.Copy.
	return make([]byte, 32768)
}

func (dbs *defaultBufferSource) Put(buf []byte) {
	// do nothing
}
