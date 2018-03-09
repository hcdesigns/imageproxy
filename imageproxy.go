// Copyright 2013 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package imageproxy provides an image proxy server.  For typical use of
// creating and using a Proxy, see cmd/imageproxy/main.go.
package imageproxy // import "willnorris.com/go/imageproxy"

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gregjones/httpcache"
	"willnorris.com/go/imageproxy/routemapping"
	tphttp "willnorris.com/go/imageproxy/third_party/http"
)

// Proxy serves image requests.
type Proxy struct {
	Client *http.Client // client used to fetch remote URLs
	Cache  Cache        // cache used to cache responses

	// Whitelist specifies a list of remote hosts that images can be
	// proxied from.  An empty list means all hosts are allowed.
	Whitelist []string

	// Referrers, when given, requires that requests to the image
	// proxy come from a referring host. An empty list means all
	// hosts are allowed.
	Referrers []string

	// DefaultBaseURL is the URL that relative remote URLs are resolved in
	// reference to.  If nil, all remote URLs specified in requests must be
	// absolute.
	DefaultBaseURL *url.URL

	// SignatureKey is the HMAC key used to verify signed requests.
	SignatureKey []byte

	// Allow images to scale beyond their original dimensions.
	ScaleUp bool

	// Timeout specifies a time limit for requests served by this Proxy.
	// If a call runs for longer than its time limit, a 504 Gateway Timeout
	// response is returned.  A Timeout of zero means no timeout.
	Timeout time.Duration

	// If true, log additional debug messages
	Verbose bool
}

var reRouteMapping *routemapping.RouteMapping

// NewProxy constructs a new proxy.  The provided http RoundTripper will be
// used to fetch remote URLs.  If nil is provided, http.DefaultTransport will
// be used.
func NewProxy(transport http.RoundTripper, cache Cache, exclusive bool, mappingURL string) *Proxy {
	if transport == nil {
		transport = http.DefaultTransport
	}
	if cache == nil {
		cache = NopCache
	}

	proxy := &Proxy{
		Cache: cache,
	}

	client := new(http.Client)
	client.Transport = &httpcache.Transport{
		Transport: &TransformingTransport{
			Transport:     transport,
			CachingClient: client,
			log: func(format string, v ...interface{}) {
				if proxy.Verbose {
					log.Printf(format, v...)
				}
			},
		},
		Cache:               cache,
		MarkCachedResponses: true,
	}

	flag.Parse()

	if exclusive && "" == strings.Trim(mappingURL, " ") {
		log.Fatalln(fmt.Errorf("Must set flag `mappingurl` when using flag `exclusive`"))
	}

	reRouteMapping = routemapping.New(exclusive)

	log.Println("Fetching latest Image Proxy Mapping JSON file. Please wait..")
	mustFetchRouteMappingChanges(mappingURL)

	var watcherContext context.Context
	go watchRouteMappingChanges(watcherContext, mappingURL)

	proxy.Client = client

	return proxy
}

// ServeHTTP handles incoming requests.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/favicon.ico" {
		return // ignore favicon requests
	}

	if r.URL.Path == "/" || r.URL.Path == "/health-check" {
		fmt.Fprint(w, "OK")
		return
	}

	var h http.Handler = http.HandlerFunc(p.serveImage)
	if p.Timeout > 0 {
		h = tphttp.TimeoutHandler(h, p.Timeout, "Gateway timeout waiting for remote resource.")
	}
	h.ServeHTTP(w, r)
}

func fetchRouteMappingChanges(watchPath string) (err error) {
	resp, err := http.Get(watchPath)
	if nil != err {
		log.Fatalln(err)
		return
	}
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if nil != err {
		log.Fatalln(err)
		return
	}
	var data map[string]string
	err = json.Unmarshal(bodyBytes, &data)
	if nil != err {
		log.Fatalln(err)
		return
	}
	reRouteMapping.Set(data)
	return err
}

func mustFetchRouteMappingChanges(watchPath string) {
	if err := fetchRouteMappingChanges(watchPath); nil != err {
		log.Println("Fetching the intial JSON Mapping failed..")
		log.Fatalln(err)
	}
}

func watchRouteMappingChanges(ctx context.Context, watchPath string) {
	for {
		time.Sleep(time.Minute)
		fetchRouteMappingChanges(watchPath)
	}
}

const invalidRequestText = "invalid request URL: %v"

// serveImage handles incoming requests for proxied images.
func (p *Proxy) serveImage(w http.ResponseWriter, r *http.Request) {
	var found bool
	for search, replace := range reRouteMapping.Get() {
		if strings.Index(r.RequestURI, "/"+search) == 0 {
			r.RequestURI = strings.Replace(r.RequestURI, "/"+search+"/", "/"+replace+"/", 1)
			r.URL.Path = strings.Replace(r.URL.Path, "/"+search+"/", "/"+replace+"/", 1)
			found = true
			break
		}
	}

	if reRouteMapping.IsExclusive() && !found {
		msg := fmt.Sprintf(invalidRequestText, r.RequestURI)
		log.Print(fmt.Errorf(msg))
		http.Error(w, msg, http.StatusNotFound)
		return
	}

	req, err := NewRequest(r, p.DefaultBaseURL)
	if err != nil {
		msg := fmt.Sprintf(invalidRequestText, err)
		log.Print(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	// assign static settings from proxy to req.Options
	req.Options.ScaleUp = p.ScaleUp

	if err := p.allowed(req); err != nil {
		log.Print(err)
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	resp, err := p.Client.Get(req.String())
	if err != nil {
		msg := fmt.Sprintf("error fetching remote image: %v", err)
		log.Print(msg)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	cached := resp.Header.Get(httpcache.XFromCache)
	if p.Verbose {
		log.Printf("request: %v (served from cache: %v)", *req, cached == "1")
	}

	copyHeader(w.Header(), resp.Header, "Cache-Control", "Last-Modified", "Expires", "Etag", "Link")

	if should304(r, resp) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	copyHeader(w.Header(), resp.Header, "Content-Length", "Content-Type")

	//Enable CORS for 3rd party applications
	w.Header().Set("Access-Control-Allow-Origin", "*")

	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// copyHeader copies header values from src to dst, adding to any existing
// values with the same header name.  If keys is not empty, only those header
// keys will be copied.
func copyHeader(dst, src http.Header, keys ...string) {
	if len(keys) == 0 {
		for k, _ := range src {
			keys = append(keys, k)
		}
	}
	for _, key := range keys {
		k := http.CanonicalHeaderKey(key)
		for _, v := range src[k] {
			dst.Add(k, v)
		}
	}
}

// allowed determines whether the specified request contains an allowed
// referrer, host, and signature.  It returns an error if the request is not
// allowed.
func (p *Proxy) allowed(r *Request) error {
	if len(p.Referrers) > 0 && !validReferrer(p.Referrers, r.Original) {
		return fmt.Errorf("request does not contain an allowed referrer: %v", r)
	}

	if len(p.Whitelist) == 0 && len(p.SignatureKey) == 0 {
		return nil // no whitelist or signature key, all requests accepted
	}

	if len(p.Whitelist) > 0 && validHost(p.Whitelist, r.URL) {
		return nil
	}

	if len(p.SignatureKey) > 0 && validSignature(p.SignatureKey, r) {
		return nil
	}

	return fmt.Errorf("request does not contain an allowed host or valid signature: %v", r)
}

// validHost returns whether the host in u matches one of hosts.
func validHost(hosts []string, u *url.URL) bool {
	for _, host := range hosts {
		if u.Host == host {
			return true
		}
		if strings.HasPrefix(host, "*.") && strings.HasSuffix(u.Host, host[2:]) {
			return true
		}
	}

	return false
}

// returns whether the referrer from the request is in the host list.
func validReferrer(hosts []string, r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("Referer"))
	if err != nil { // malformed or blank header, just deny
		return false
	}

	return validHost(hosts, u)
}

// validSignature returns whether the request signature is valid.
func validSignature(key []byte, r *Request) bool {
	sig := r.Options.Signature
	if m := len(sig) % 4; m != 0 { // add padding if missing
		sig += strings.Repeat("=", 4-m)
	}

	got, err := base64.URLEncoding.DecodeString(sig)
	if err != nil {
		log.Printf("error base64 decoding signature %q", r.Options.Signature)
		return false
	}

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(r.URL.String()))
	want := mac.Sum(nil)

	return hmac.Equal(got, want)
}

// should304 returns whether we should send a 304 Not Modified in response to
// req, based on the response resp.  This is determined using the last modified
// time and the entity tag of resp.
func should304(req *http.Request, resp *http.Response) bool {
	// TODO(willnorris): if-none-match header can be a comma separated list
	// of multiple tags to be matched, or the special value "*" which
	// matches all etags
	etag := resp.Header.Get("Etag")
	if etag != "" && etag == req.Header.Get("If-None-Match") {
		return true
	}

	lastModified, err := time.Parse(time.RFC1123, resp.Header.Get("Last-Modified"))
	if err != nil {
		return false
	}
	ifModSince, err := time.Parse(time.RFC1123, req.Header.Get("If-Modified-Since"))
	if err != nil {
		return false
	}
	if lastModified.Before(ifModSince) || lastModified.Equal(ifModSince) {
		return true
	}

	return false
}

// TransformingTransport is an implementation of http.RoundTripper that
// optionally transforms images using the options specified in the request URL
// fragment.
type TransformingTransport struct {
	// Transport is the underlying http.RoundTripper used to satisfy
	// non-transform requests (those that do not include a URL fragment).
	Transport http.RoundTripper

	// CachingClient is used to fetch images to be resized.  This client is
	// used rather than Transport directly in order to ensure that
	// responses are properly cached.
	CachingClient *http.Client

	log func(format string, v ...interface{})
}

// RoundTrip implements the http.RoundTripper interface.
func (t *TransformingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Fragment == "" {
		// normal requests pass through
		if t.log != nil {
			t.log("fetching remote URL: %v", req.URL)
		}
		return t.Transport.RoundTrip(req)
	}

	u := *req.URL
	u.Fragment = ""
	resp, err := t.CachingClient.Get(u.String())
	if err != nil {
		return nil, err
	}

	if should304(req, resp) {
		// bare 304 response, full response will be used from cache
		return &http.Response{StatusCode: http.StatusNotModified}, nil
	}

	defer resp.Body.Close()
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	opt := ParseOptions(req.URL.Fragment)

	img, err := Transform(b, opt)
	if err != nil {
		log.Printf("error transforming image: %v", err)
		img = b
	}

	// replay response with transformed image and updated content length
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "%s %s\n", resp.Proto, resp.Status)
	resp.Header.WriteSubset(buf, map[string]bool{
		"Content-Length": true,
		// exclude Content-Type header if the format may have changed during transformation
		"Content-Type": opt.Format != "" || resp.Header.Get("Content-Type") == "image/webp" || resp.Header.Get("Content-Type") == "image/tiff",
	})
	fmt.Fprintf(buf, "Content-Length: %d\n\n", len(img))
	buf.Write(img)

	return http.ReadResponse(bufio.NewReader(buf), req)
}
