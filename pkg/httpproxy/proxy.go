package httpproxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"

	v1 "github.com/rancher/types/apis/core/v1"
	"github.com/rancher/types/config"
	"github.com/sirupsen/logrus"
)

const (
	ForwardProto = "X-Forwarded-Proto"
	APIAuth      = "X-API-Auth-Header"
	CattleAuth   = "X-API-CattleAuth-Header"
	AuthHeader   = "Authorization"
	SetCookie    = "Set-Cookie"
	Cookie       = "Cookie"
	APISetCookie = "X-Api-Set-Cookie-Header"
	APICookie    = "X-Api-Cookie-Header"
)

var (
	httpStart  = regexp.MustCompile("^http:/([^/])")
	httpsStart = regexp.MustCompile("^https:/([^/])")
	badHeaders = map[string]bool{
		"host":                    true,
		"transfer-encoding":       true,
		"content-length":          true,
		"x-api-auth-header":       true,
		"x-api-cattleauth-header": true,
		"cf-connecting-ip":        true,
		"cf-ray":                  true,
		"impersonate-user":        true,
		"impersonate-group":       true,
	}
)

type Supplier func() []string

type proxy struct {
	prefix             string
	validHostsSupplier Supplier
	credentials        v1.SecretInterface
}

func (p *proxy) isAllowed(host string) bool {
	for _, valid := range p.validHostsSupplier() {
		if valid == host {
			return true
		}

		if strings.HasPrefix(valid, "*") && strings.HasSuffix(host, valid[1:]) {
			return true
		}
	}

	return false
}

func NewProxy(prefix string, validHosts Supplier, scaledContext *config.ScaledContext) http.Handler {
	p := proxy{
		prefix:             prefix,
		validHostsSupplier: validHosts,
		credentials:        scaledContext.Core.Secrets(""),
	}

	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			if err := p.proxy(req); err != nil {
				logrus.Infof("Failed to proxy %v: %v", req, err)
			}
		},
		ModifyResponse: replaceSetCookies,
	}
}

func replaceSetCookies(res *http.Response) error {
	res.Header.Del(APISetCookie)
	// There may be multiple set cookies
	for _, setCookie := range res.Header[SetCookie] {
		res.Header.Add(APISetCookie, setCookie)
	}
	res.Header.Del(SetCookie)
	return nil
}

func (p *proxy) proxy(req *http.Request) error {
	path := req.URL.String()
	index := strings.Index(path, p.prefix)
	destPath := path[index+len(p.prefix):]

	if httpsStart.MatchString(destPath) {
		destPath = httpsStart.ReplaceAllString(destPath, "https://$1")
	} else if httpStart.MatchString(destPath) {
		destPath = httpStart.ReplaceAllString(destPath, "http://$1")
	} else {
		destPath = "https://" + destPath
	}

	destURL, err := url.Parse(destPath)
	if err != nil {
		return err
	}

	destURL.RawQuery = req.URL.RawQuery

	if !p.isAllowed(destURL.Host) {
		return fmt.Errorf("invalid host: %v", destURL.Host)
	}

	headerCopy := http.Header{}

	if req.TLS != nil {
		headerCopy.Set(ForwardProto, "https")
	}
	auth := req.Header.Get(APIAuth)
	cAuth := req.Header.Get(CattleAuth)
	for name, value := range req.Header {
		if badHeaders[strings.ToLower(name)] {
			continue
		}

		copy := make([]string, len(value))
		for i := range value {
			copy[i] = strings.TrimPrefix(value[i], "rancher:")
		}
		headerCopy[name] = copy
	}

	req.Host = destURL.Hostname()
	req.URL = destURL
	req.Header = headerCopy

	if auth != "" { // non-empty AuthHeader is noop
		req.Header.Set(AuthHeader, auth)
	} else if cAuth != "" {
		// setting CattleAuthHeader will replace credential id with secret data
		// and generate signature
		signer := newSigner(cAuth)
		if signer != nil {
			return signer.sign(req, p.credentials, cAuth)
		}
		req.Header.Set(AuthHeader, cAuth)
	}

	replaceCookies(req)

	return nil
}

func replaceCookies(req *http.Request) {
	// Do not forward rancher cookies to third parties
	req.Header.Del(Cookie)
	// Allow client to use their own cookies with Cookie header
	if cookie := req.Header.Get(APICookie); cookie != "" {
		req.Header.Set(Cookie, cookie)
		req.Header.Del(APICookie)
	}
}
