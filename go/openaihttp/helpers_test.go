package openaihttp

import (
	"crypto/x509"
	"net/http/httptest"
	"net/url"
	"testing"
)

func certPool(servers ...*httptest.Server) *x509.CertPool {
	pool := x509.NewCertPool()
	for _, s := range servers {
		pool.AddCert(s.Certificate())
	}
	return pool
}

func hostport(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}
