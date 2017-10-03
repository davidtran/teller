package proxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"time"

	"github.com/NYTimes/gziphandler"
	"github.com/didip/tollbooth"
	"github.com/unrolled/secure"
	"golang.org/x/crypto/acme/autocert"

	"github.com/skycoin/skycoin/src/cipher"
	"github.com/skycoin/teller/src/daemon"
	"github.com/skycoin/teller/src/util/httputil"
	"github.com/skycoin/teller/src/util/logger"
)

const (
	proxyRequestTimeout = time.Second * 30

	shutdownTimeout = time.Second * 5

	// https://blog.cloudflare.com/the-complete-guide-to-golang-net-http-timeouts/
	// The timeout configuration is necessary for public servers, or else
	// connections will be used up
	serverReadTimeout  = time.Second * 10
	serverWriteTimeout = time.Second * 60
	serverIdleTimeout  = time.Second * 120

	// Directory where cached SSL certs from Let's Encrypt are stored
	tlsAutoCertCache = "cert-cache"
)

type Throttle struct {
	Max      int64
	Duration time.Duration
}

type httpServ struct {
	logger.Logger
	Addr          string
	HTTPSAddr     string
	StaticDir     string
	HTMLInterface bool
	StartAt       time.Time
	AutoTLSHost   string
	TLSCert       string
	TLSKey        string
	Gateway       *gateway
	WithoutTeller bool

	Throttle Throttle

	httpListener  *http.Server
	httpsListener *http.Server
	quit          chan struct{}
}

func (hs *httpServ) Run() error {
	hs.Println("Http service start")
	if hs.Addr != "" {
		hs.Println("Http service address:", hs.Addr)
	}
	if hs.HTTPSAddr != "" {
		hs.Println("Https service address:", hs.HTTPSAddr)
	}
	defer hs.Debugln("Http service closed")

	hs.quit = make(chan struct{})

	var mux http.Handler = hs.setupMux()

	allowedHosts := []string{} // empty array means all hosts allowed
	sslHost := ""
	if hs.AutoTLSHost == "" {
		// Note: if AutoTLSHost is not set, but HTTPSAddr is set, then
		// http will redirect to the HTTPSAddr listening IP, which would be
		// either 127.0.0.1 or 0.0.0.0
		// When running behind a DNS name, make sure to set AutoTLSHost
		sslHost = hs.HTTPSAddr
	} else {
		sslHost = hs.AutoTLSHost
		// When using -auto-tls-host,
		// which implies automatic Let's Encrypt SSL cert generation in production,
		// restrict allowed hosts to that host.
		allowedHosts = []string{hs.AutoTLSHost}
	}

	if len(allowedHosts) == 0 {
		hs.Println("Allowed hosts: all")
	} else {
		hs.Println("Allowed hosts:", allowedHosts)
	}

	hs.Println("SSL Host:", sslHost)

	secureMiddleware := configureSecureMiddleware(sslHost, allowedHosts)
	mux = secureMiddleware.Handler(mux)

	if hs.Addr != "" {
		hs.httpListener = setupHTTPListener(hs.Addr, mux)
	}

	handleListenErr := func(f func() error) error {
		if err := f(); err != nil {
			select {
			case <-hs.quit:
				return nil
			default:
				hs.Println("ListenAndServe or ListenAndServeTLS error:", err)
				return fmt.Errorf("http serve failed: %v", err)
			}
		}
		return nil
	}

	if hs.HTTPSAddr != "" {
		hs.Println("Using TLS")

		hs.httpsListener = setupHTTPListener(hs.HTTPSAddr, mux)

		tlsCert := hs.TLSCert
		tlsKey := hs.TLSKey

		if hs.AutoTLSHost != "" {
			hs.Println("Using Let's Encrypt autocert with host", hs.AutoTLSHost)
			// https://godoc.org/golang.org/x/crypto/acme/autocert
			// https://stackoverflow.com/a/40494806
			certManager := autocert.Manager{
				Prompt:     autocert.AcceptTOS,
				HostPolicy: autocert.HostWhitelist(hs.AutoTLSHost),
				Cache:      autocert.DirCache(tlsAutoCertCache),
			}

			hs.httpsListener.TLSConfig = &tls.Config{
				GetCertificate: certManager.GetCertificate,
			}

			// These will be autogenerated by the autocert middleware
			tlsCert = ""
			tlsKey = ""
		}

		errC := make(chan error)

		if hs.Addr == "" {
			return handleListenErr(func() error {
				return hs.httpsListener.ListenAndServeTLS(tlsCert, tlsKey)
			})
		}
		return handleListenErr(func() error {
			var wg sync.WaitGroup
			wg.Add(2)

			go func() {
				defer wg.Done()
				if err := hs.httpsListener.ListenAndServeTLS(tlsCert, tlsKey); err != nil {
					hs.Println("ListenAndServeTLS error:", err)
					errC <- err
				}
			}()

			go func() {
				defer wg.Done()
				if err := hs.httpListener.ListenAndServe(); err != nil {
					hs.Println("ListenAndServe error:", err)
					errC <- err
				}
			}()

			done := make(chan struct{})

			go func() {
				wg.Wait()
				close(done)
			}()

			select {
			case err := <-errC:
				return err
			case <-hs.quit:
				return nil
			case <-done:
				return nil
			}
		})
	}

	return handleListenErr(func() error {
		return hs.httpListener.ListenAndServe()
	})

}

func configureSecureMiddleware(sslHost string, allowedHosts []string) *secure.Secure {
	sslRedirect := true
	if sslHost == "" {
		sslRedirect = false
	}

	return secure.New(secure.Options{
		AllowedHosts: allowedHosts,
		SSLRedirect:  sslRedirect,
		SSLHost:      sslHost,

		// https://developer.mozilla.org/en-US/docs/Web/HTTP/CSP
		// FIXME: Web frontend code has inline styles, CSP doesn't work yet
		// ContentSecurityPolicy: "default-src 'self'",

		// Set HSTS to one year, for this domain only, do not add to chrome preload list
		// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Strict-Transport-Security
		STSSeconds:           31536000, // 1 year
		STSIncludeSubdomains: false,
		STSPreload:           false,

		// Deny use in iframes
		// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/X-Frame-Options
		FrameDeny: true,

		// Disable MIME sniffing in browsers
		// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/X-Content-Type-Options
		ContentTypeNosniff: true,

		// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/X-XSS-Protection
		BrowserXssFilter: true,

		// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Referrer-Policy
		// "same-origin" is invalid in chrome
		ReferrerPolicy: "no-referrer",
	})
}

func setupHTTPListener(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  serverReadTimeout,
		WriteTimeout: serverWriteTimeout,
		IdleTimeout:  serverIdleTimeout,
	}
}

func (hs *httpServ) setupMux() *http.ServeMux {
	mux := http.NewServeMux()

	handleAPI := func(path string, f http.HandlerFunc) {
		mux.Handle(path, gziphandler.GzipHandler(rateLimiter(hs.Throttle, httputil.LogHandler(hs.Logger, f))))
	}

	if !hs.WithoutTeller {
		// API Methods
		handleAPI("/api/bind", BindHandler(hs))
		handleAPI("/api/status", StatusHandler(hs))
	}

	// Static files
	if hs.HTMLInterface {
		mux.Handle("/", gziphandler.GzipHandler(http.FileServer(http.Dir(hs.StaticDir))))
	}

	return mux
}

func rateLimiter(thr Throttle, hd http.HandlerFunc) http.Handler {
	return tollbooth.LimitFuncHandler(tollbooth.NewLimiter(thr.Max, thr.Duration), hd)
}

func (hs *httpServ) Shutdown() {
	if hs.quit != nil {
		close(hs.quit)
	}

	shutdown := func(proto string, ln *http.Server) {
		if ln == nil {
			return
		}
		hs.Printf("Shutting down %s server, %s timeout\n", proto, shutdownTimeout)

		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := ln.Shutdown(ctx); err != nil {
			hs.Println("HTTP server shutdown error:", err)
		}
	}

	shutdown("HTTP", hs.httpListener)
	shutdown("HTTPS", hs.httpsListener)

	hs.quit = nil
}

// BindHandler binds skycoin address with a bitcoin address
// Method: POST
// Accept: application/json
// URI: /api/bind
// Args:
//    {"skyaddr": "..."}
func BindHandler(srv *httpServ) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept", "application/json")

		if !validMethod(w, r, srv.Gateway, []string{http.MethodPost}) {
			return
		}
		if r.Header.Get("Content-Type") != "application/json" {
			errorResponse(w, srv.Gateway, http.StatusUnsupportedMediaType)
			return
		}

		userBindReq := &bindRequest{}
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&userBindReq); err != nil {
			errorResponse(w, srv.Gateway, http.StatusBadRequest, "Invalid json request body:", err)
			return
		}
		defer r.Body.Close()

		if userBindReq.SkyAddr == "" {
			errorResponse(w, srv.Gateway, http.StatusBadRequest, "Missing skyaddr")
			return
		}

		if !verifySkycoinAddress(w, srv.Gateway, userBindReq.SkyAddr) {
			return
		}

		if !readyToStart(w, srv.Gateway, srv.StartAt) {
			return
		}

		cxt, cancel := context.WithTimeout(r.Context(), proxyRequestTimeout)
		defer cancel()

		daemonBindReq := daemon.BindRequest{SkyAddress: userBindReq.SkyAddr}

		srv.Println("Sending BindRequest to teller, skyaddr", userBindReq.SkyAddr)

		rsp, err := srv.Gateway.BindAddress(cxt, &daemonBindReq)
		if err != nil {
			handleGatewayResponseError(w, srv.Gateway, err)
			return
		}

		srv.Printf("Received response to BindRequest: %+v\n", *rsp)

		if rsp.Error != "" {
			httputil.ErrResponse(w, http.StatusBadRequest, rsp.Error)
			srv.Println(rsp.Error)
			return
		}

		if err := httputil.JSONResponse(w, makeBindHTTPResponse(*rsp)); err != nil {
			srv.Println(err)
		}
	}
}

type bindRequest struct {
	SkyAddr string `json:"skyaddr"`
}

// StatusHandler returns the deposit status of specific skycoin address
// Method: GET
// URI: /api/status
// Args:
//     skyaddr
func StatusHandler(srv *httpServ) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validMethod(w, r, srv.Gateway, []string{http.MethodGet}) {
			return
		}

		skyAddr := r.URL.Query().Get("skyaddr")
		if skyAddr == "" {
			errorResponse(w, srv.Gateway, http.StatusBadRequest, "Missing skyaddr")
			return
		}

		if !verifySkycoinAddress(w, srv.Gateway, skyAddr) {
			return
		}

		if !readyToStart(w, srv.Gateway, srv.StartAt) {
			return
		}

		cxt, cancel := context.WithTimeout(r.Context(), proxyRequestTimeout)
		defer cancel()

		stReq := daemon.StatusRequest{SkyAddress: skyAddr}

		srv.Println("Sending StatusRequest to teller, skyaddr", skyAddr)

		rsp, err := srv.Gateway.GetDepositStatuses(cxt, &stReq)
		if err != nil {
			handleGatewayResponseError(w, srv.Gateway, err)
			return
		}

		srv.Printf("Received response to StatusRequest: %+v\n", *rsp)

		if rsp.Error != "" {
			httputil.ErrResponse(w, http.StatusBadRequest, rsp.Error)
			srv.Println(rsp.Error)
			return
		}

		if err := httputil.JSONResponse(w, makeStatusHTTPResponse(*rsp)); err != nil {
			srv.Println(err)
		}
	}
}

func readyToStart(w http.ResponseWriter, gw gatewayer, startAt time.Time) bool {
	if time.Now().UTC().After(startAt.UTC()) {
		return true
	}

	msg := fmt.Sprintf("Event starts at %v", startAt)
	httputil.ErrResponse(w, http.StatusForbidden, msg)
	gw.Println(http.StatusForbidden, msg)

	return false
}

func validMethod(w http.ResponseWriter, r *http.Request, gw gatewayer, allowed []string) bool {
	for _, m := range allowed {
		if r.Method == m {
			return true
		}
	}

	w.Header().Set("Allow", strings.Join(allowed, ", "))

	status := http.StatusMethodNotAllowed
	errorResponse(w, gw, status, "Invalid request method:", r.Method)

	return false
}

func verifySkycoinAddress(w http.ResponseWriter, gw gatewayer, skyAddr string) bool {
	if _, err := cipher.DecodeBase58Address(skyAddr); err != nil {
		msg := fmt.Sprintf("Invalid skycoin address: %v", err)
		httputil.ErrResponse(w, http.StatusBadRequest, msg)
		gw.Println(http.StatusBadRequest, "Invalid skycoin address:", err, skyAddr)
		return false
	}
	return true
}

func handleGatewayResponseError(w http.ResponseWriter, gw gatewayer, err error) {
	if err == nil {
		return
	}

	if err == context.DeadlineExceeded {
		errorResponse(w, gw, http.StatusRequestTimeout)
		return
	}

	errorResponse(w, gw, http.StatusInternalServerError, err)
	return
}

func errorResponse(w http.ResponseWriter, gw gatewayer, code int, msgs ...interface{}) {
	gw.Println(append([]interface{}{code, http.StatusText(code)}, msgs...)...)
	httputil.ErrResponse(w, code)
}
