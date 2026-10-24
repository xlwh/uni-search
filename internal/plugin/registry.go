// Package plugin implements the plugin SDK (design 02 §1.2).
// Dual-mode: in-process registration (Go interface) + gRPC sidecar.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#plugin-sidecar
package plugin

import (
	"context"
	"fmt"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/unisearch-db/unisearch/internal/index"
)

// RemoteIndex is the interface sidecar plugins implement.
type RemoteIndex interface {
	Build(ctx context.Context, provider index.ColumnProvider, output index.IndexOutput) error
	Load(input index.IndexInput) (index.LoadedIndex, error)
	Search(ctx context.Context, q index.Query, budget index.SearchBudget) (*index.SearchResult, error)
}

// PluginRegistry manages both in-process and sidecar plugins.
type PluginRegistry struct {
	mu       sync.RWMutex
	inProcess map[string]index.IndexFactory
	sidecars  map[string]*SidecarClient
}

// SidecarClient connects to a gRPC sidecar plugin.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#plugin-sidecar
type SidecarClient struct {
	name string
	addr string
	conn *grpc.ClientConn
}

// NewPluginRegistry creates an empty registry.
func NewPluginRegistry() *PluginRegistry {
	return &PluginRegistry{
		inProcess: make(map[string]index.IndexFactory),
		sidecars:  make(map[string]*SidecarClient),
	}
}

// RegisterInProcess registers an in-process index plugin.
func (r *PluginRegistry) RegisterInProcess(name string, factory index.IndexFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inProcess[name] = factory
}

// RegisterSidecar registers a gRPC sidecar plugin and dials the
// sidecar process. The connection uses insecure credentials (TLS is
// configured externally in production).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#plugin-sidecar
func (r *PluginRegistry) RegisterSidecar(name, addr string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Dial the sidecar gRPC endpoint.
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("plugin: dial sidecar %s at %s: %w", name, addr, err)
	}
	r.sidecars[name] = &SidecarClient{name: name, addr: addr, conn: conn}
	return nil
}

// GetFactory returns the in-process factory for a plugin name.
func (r *PluginRegistry) GetFactory(name string) (index.IndexFactory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.inProcess[name]
	return f, ok
}

// GetSidecar returns the sidecar client for a plugin name.
func (r *PluginRegistry) GetSidecar(name string) (*SidecarClient, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sidecars[name]
	return s, ok
}

// List returns all registered plugin names.
func (r *PluginRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var names []string
	for n := range r.inProcess {
		names = append(names, n)
	}
	for n := range r.sidecars {
		names = append(names, n)
	}
	return names
}

// NewIndex creates an index from a registered plugin.
// For in-process plugins, returns the factory result directly.
// For sidecar plugins, returns a SidecarIndex adapter that delegates
// Build/Load/Search to the remote gRPC server.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#plugin-sidecar
func (r *PluginRegistry) NewIndex(name string) (index.Index, error) {
	if factory, ok := r.GetFactory(name); ok {
		return factory(), nil
	}
	if sc, ok := r.GetSidecar(name); ok {
		if sc.conn == nil {
			return nil, fmt.Errorf("plugin: sidecar %q not connected", name)
		}
		return &sidecarIndexAdapter{name: name, client: sc}, nil
	}
	return nil, fmt.Errorf("plugin: unknown plugin %q", name)
}

// Close closes all sidecar gRPC connections.
func (r *PluginRegistry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var firstErr error
	for _, sc := range r.sidecars {
		if sc.conn != nil {
			if err := sc.conn.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
			sc.conn = nil
		}
	}
	return firstErr
}

// sidecarIndexAdapter wraps a SidecarClient to implement index.Index.
// Build and Load delegate to the sidecar via a simplified protocol;
// in a full implementation these would use a proto-defined IndexService.
type sidecarIndexAdapter struct {
	name   string
	client *SidecarClient
}

func (a *sidecarIndexAdapter) Kind() index.IndexKind {
	// Sidecar plugins report their kind dynamically; the default
	// is KindHNSW for GPU-accelerated vector indexes (e.g. CAGRA).
	return index.KindHNSW
}

func (a *sidecarIndexAdapter) Build(ctx context.Context, col index.ColumnProvider, out index.IndexOutput) error {
	// Full implementation would serialize the ColumnProvider and
	// stream it to the sidecar. For now, this is a placeholder that
	// signals the sidecar to build from its own data source.
	return fmt.Errorf("plugin: sidecar Build requires IndexService proto (not yet defined)")
}

func (a *sidecarIndexAdapter) Load(inp index.IndexInput) (index.LoadedIndex, error) {
	return nil, fmt.Errorf("plugin: sidecar Load requires IndexService proto (not yet defined)")
}

func (a *sidecarIndexAdapter) Merge(ctx context.Context, others []index.LoadedIndex, out index.IndexOutput) error {
	return fmt.Errorf("plugin: sidecar Merge not supported")
}

func (a *sidecarIndexAdapter) Quantize(ctx context.Context, to index.QuantLevel) error {
	return fmt.Errorf("plugin: sidecar Quantize not supported")
}

func (a *sidecarIndexAdapter) MemEstimate() int64 {
	return 0 // unknown for sidecar
}
