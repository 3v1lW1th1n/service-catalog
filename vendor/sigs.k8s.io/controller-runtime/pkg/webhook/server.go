/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package webhook

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"path"
	"strconv"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/runtime/inject"
	"sigs.k8s.io/controller-runtime/pkg/webhook/internal/metrics"
)

const (
	certName = "tls.crt"
	keyName  = "tls.key"
)

// DefaultPort is the default port that the webhook server serves.
var DefaultPort = 443

// Server is an admission webhook server that can serve traffic and
// generates related k8s resources for deploying.
type Server struct {
	// Host is the address that the server will listen on.
	// Defaults to "" - all addresses.
	Host string

	// Port is the port number that the server will serve.
	// It will be defaulted to 443 if unspecified.
	Port int

	// CertDir is the directory that contains the server key and certificate.
	// If using FSCertWriter in Provisioner, the server itself will provision the certificate and
	// store it in this directory.
	// If using SecretCertWriter in Provisioner, the server will provision the certificate in a secret,
	// the user is responsible to mount the secret to the this location for the server to consume.
	CertDir string

	// WebhookMux is the multiplexer that handles different webhooks.
	WebhookMux *http.ServeMux

	// webhooks keep track of all registered webhooks for dependency injection,
	// and to provide better panic messages on duplicate webhook registration.
	webhooks map[string]http.Handler

	// setFields allows injecting dependencies from an external source
	setFields inject.Func

	// defaultingOnce ensures that the default fields are only ever set once.
	defaultingOnce sync.Once
}

// setDefaults does defaulting for the Server.
func (s *Server) setDefaults() {
	s.webhooks = map[string]http.Handler{}
	if s.WebhookMux == nil {
		s.WebhookMux = http.NewServeMux()
	}

	if s.Port <= 0 {
		s.Port = DefaultPort
	}

	if len(s.CertDir) == 0 {
		s.CertDir = path.Join("/tmp", "k8s-webhook-server", "serving-certs")
	}
}

// Register marks the given webhook as being served at the given path.
// It panics if two hooks are registered on the same path.
func (s *Server) Register(path string, hook http.Handler) {
	s.defaultingOnce.Do(s.setDefaults)
	_, found := s.webhooks[path]
	if found {
		panic(fmt.Errorf("can't register duplicate path: %v", path))
	}
	// TODO(directxman12): call setfields if we've already started the server
	s.webhooks[path] = hook
	s.WebhookMux.Handle(path, instrumentedHook(path, hook))
}

// instrumentedHook adds some instrumentation on top of the given webhook.
func instrumentedHook(path string, hookRaw http.Handler) http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		startTS := time.Now()
		defer func() { metrics.RequestLatency.WithLabelValues(path).Observe(time.Now().Sub(startTS).Seconds()) }()
		hookRaw.ServeHTTP(resp, req)

		// TODO(directxman12): add back in metric about total requests broken down by result?
	})
}

// Start runs the server.
// It will install the webhook related resources depend on the server configuration.
func (s *Server) Start(stop <-chan struct{}) error {
	s.defaultingOnce.Do(s.setDefaults)

	baseHookLog := log.WithName("webhooks")
	// inject fields here as opposed to in Register so that we're certain to have our setFields
	// function available.
	for hookPath, webhook := range s.webhooks {
		if err := s.setFields(webhook); err != nil {
			return err
		}

		// NB(directxman12): we don't propagate this further by wrapping setFields because it's
		// unclear if this is how we want to deal with log propagation.  In this specific instance,
		// we want to be able to pass a logger to webhooks because they don't know their own path.
		if _, err := inject.LoggerInto(baseHookLog.WithValues("webhook", hookPath), webhook); err != nil {
			return err
		}
	}

	// TODO: watch the cert dir. Reload the cert if it changes
	cert, err := tls.LoadX509KeyPair(path.Join(s.CertDir, certName), path.Join(s.CertDir, keyName))
	if err != nil {
		return err
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2"},
	}

	listener, err := tls.Listen("tcp", net.JoinHostPort(s.Host, strconv.Itoa(int(s.Port))), cfg)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler: s.WebhookMux,
	}

	idleConnsClosed := make(chan struct{})
	go func() {
		<-stop

		// TODO: use a context with reasonable timeout
		if err := srv.Shutdown(context.Background()); err != nil {
			// Error from closing listeners, or context timeout
			log.Error(err, "error shutting down the HTTP server")
		}
		close(idleConnsClosed)
	}()

	err = srv.Serve(listener)
	if err != nil && err != http.ErrServerClosed {
		return err
	}

	<-idleConnsClosed
	return nil
}

// InjectFunc injects the field setter into the server.
func (s *Server) InjectFunc(f inject.Func) error {
	s.setFields = f
	return nil
}
