package hub

import (
	"context"
	"strings"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/lubobill1990/longarm/internal/hub/store"
)

// acmeMetaPrefix starts the meta keys of the https listener's ACME state
// (Let's Encrypt account key, certificates; autocert's cache keys).
const acmeMetaPrefix = "acme/"

// acmeCache is autocert's cache in the database: replicated with it, so a
// hub whose disk is gone (a container) keeps its certificate.
type acmeCache struct {
	st *store.Store
	h  *Hub // telemetry of new certificates (nil = none)
}

func (c acmeCache) Get(_ context.Context, key string) ([]byte, error) {
	v, err := c.st.Meta(acmeMetaPrefix + key)
	if err != nil {
		return nil, err
	}
	if v == "" {
		return nil, autocert.ErrCacheMiss
	}
	return []byte(v), nil
}

func (c acmeCache) Put(_ context.Context, key string, data []byte) error {
	err := c.st.SetMeta(acmeMetaPrefix+key, string(data))
	if c.h != nil && !strings.Contains(key, "+") { // a certificate (not the account key)
		c.h.event(context.Background(), "acme.certificate", map[string]string{"stored": boolStr(err == nil)})
	}
	return err
}

func (c acmeCache) Delete(_ context.Context, key string) error {
	return c.st.DeleteMeta(acmeMetaPrefix + key)
}

// acmeManager gets and renews the https listener's certificate for the
// host of public_url (Config.HTTPS).
func (h *Hub) acmeManager() *autocert.Manager {
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      acmeCache{h.store, h},
		HostPolicy: autocert.HostWhitelist(h.cfg.ACMEHost()),
		Email:      h.cfg.ACMEEmail,
	}
	if h.cfg.ACMEDirectory != "" {
		m.Client = &acme.Client{DirectoryURL: h.cfg.ACMEDirectory}
	}
	return m
}
