package hostsvc

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/taills/moduless/manifest"
	pb "github.com/taills/moduless/proto/plugin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Which permission gates which call.
//
// The gate itself is covered elsewhere — tests/plugin_e2e_test.go proves it
// holds across the process boundary, tests/degraded_test.go proves a refusal is
// not confusable with a Core that has no database. What none of them check is
// the mapping: that Enqueue is gated on queue rather than on cache, and that
// every call is gated on something.
//
// A wrong entry here is not a broken mechanism, it is a misplaced boundary — a
// plugin approved for the cache reaching the queue, with every test still
// green because the gate works perfectly on the wrong permission.
//
// An empty string means the call needs no permission. Three do: a plugin can
// always read its own configuration and emit its own logs and metrics, because
// refusing those would leave an author unable to see why anything else was
// refused.
var requiredPermission = map[string]string{
	"Put":       manifest.PermDB,
	"Get":       manifest.PermDB,
	"Delete":    manifest.PermDB,
	"Find":      manifest.PermDB,
	"Query":     manifest.PermDB,
	"Aggregate": manifest.PermDB,

	"BatchWrite": manifest.PermDB,
	"BeginTx":    manifest.PermDBTx,
	"CommitTx":   manifest.PermDBTx,
	"RollbackTx": manifest.PermDBTx,

	"Enqueue": manifest.PermQueue,
	"Consume": manifest.PermQueue,
	"Ack":     manifest.PermQueue,
	"Nack":    manifest.PermQueue,

	"CacheGet":    manifest.PermCache,
	"CacheSet":    manifest.PermCache,
	"CacheDelete": manifest.PermCache,

	"AcquireLock": manifest.PermLock,
	"RenewLock":   manifest.PermLock,
	"ReleaseLock": manifest.PermLock,

	"PutFile":               manifest.PermFilesWrite,
	"DeleteFile":            manifest.PermFilesWrite,
	"GenerateDownloadToken": manifest.PermFilesRead,
	"GetFileMetadata":       manifest.PermFilesRead,

	"Fetch": manifest.PermHTTPEgress,

	"Publish":   manifest.PermEvents,
	"Subscribe": manifest.PermEvents,

	"GetConfig":    "",
	"Log":          "",
	"RecordMetric": "",
}

// everyPermission is what a plugin could be granted at most.
var everyPermission = []string{
	manifest.PermDB, manifest.PermDBTx, manifest.PermQueue, manifest.PermCache,
	manifest.PermLock, manifest.PermCron, manifest.PermEvents,
	manifest.PermFilesRead, manifest.PermFilesWrite, manifest.PermHTTPEgress,
	manifest.PermFilterAuthenticate,
}

func TestEveryRPCIsGatedOnItsOwnPermission(t *testing.T) {
	// The interface, not a hand-written list: a new RPC arrives here the moment
	// it is added to the service, and fails for want of a table entry rather
	// than being silently ungated.
	iface := reflect.TypeOf((*pb.HostServicesServer)(nil)).Elem()

	checked := 0
	for i := range iface.NumMethod() {
		name := iface.Method(i).Name
		if strings.HasPrefix(name, "mustEmbedUnimplemented") {
			continue
		}

		want, declared := requiredPermission[name]
		if !declared {
			t.Errorf("%s is a host RPC with no entry in requiredPermission. "+
				"Say which permission it needs — or \"\" if it deliberately "+
				"needs none — so that the boundary is stated somewhere other "+
				"than in the implementation it is supposed to check.", name)
			continue
		}
		checked++

		if want == "" {
			assertReachable(t, name, nil)
			continue
		}
		// Everything except the one it should need. Granting nothing would only
		// prove it is gated on something; this proves it is gated on this.
		assertDenied(t, name, want, without(everyPermission, want))
		// And only the one it needs, so an over-gated call shows up too.
		assertReachable(t, name, []string{want})
	}

	if checked < 25 {
		t.Fatalf("only reached %d RPCs; the walk over HostServicesServer is not "+
			"finding them", checked)
	}
	t.Logf("checked %d host RPCs against their declared permission", checked)
}

func assertDenied(t *testing.T, method, want string, granted []string) {
	t.Helper()

	err, panicked := callRPC(t, New("plugin-under-test", granted, Deps{}), method)
	if panicked != nil {
		t.Errorf("%s panicked instead of refusing: %v\nA gated call has to return "+
			"before it reads anything, or the gate is not the first thing that "+
			"happens.", method, panicked)
		return
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("%s with every permission except %q returned %v, want PermissionDenied: "+
			"it is not gated on the permission it is supposed to need", method, want, err)
		return
	}
	// The message has to name the missing permission, or an author holding ten
	// of them has to guess which one to add.
	if !strings.Contains(err.Error(), want) {
		t.Errorf("%s was denied without naming %q: %v", method, want, err)
	}
}

func assertReachable(t *testing.T, method string, granted []string) {
	t.Helper()

	err, panicked := callRPC(t, New("plugin-under-test", granted, Deps{}), method)
	if panicked != nil {
		// It reached its own body on zero-valued arguments — Log is a client
		// stream and starts by reading one. That is the evidence wanted here:
		// a gate returns a refusal, it does not panic, so getting this far
		// means nothing refused the call.
		return
	}
	if status.Code(err) == codes.PermissionDenied {
		t.Errorf("%s was refused while holding exactly what it needs (%v): %v\n"+
			"An over-gated call is as wrong as an ungated one — it asks an "+
			"author for a permission the reviewer then has to justify.", method, granted, err)
	}
	// Anything else — Unavailable for a Core with no database behind it — is
	// expected here and is not what this test is about.
}

// callRPC invokes one host RPC with zero-valued arguments. The permission check
// runs before any of them are read, which is the property being relied on; a
// method that looked at its request first would panic here and say so.
func callRPC(t *testing.T, s *Server, method string) (err error, panicked any) {
	t.Helper()

	m := reflect.ValueOf(s).MethodByName(method)
	if !m.IsValid() {
		t.Fatalf("%s is on the service interface but not on Server", method)
	}

	typ := m.Type()
	args := make([]reflect.Value, typ.NumIn())
	for i := range typ.NumIn() {
		if typ.In(i) == reflect.TypeOf((*context.Context)(nil)).Elem() {
			args[i] = reflect.ValueOf(context.Background())
			continue
		}
		args[i] = reflect.Zero(typ.In(i))
	}

	func() {
		defer func() { panicked = recover() }()
		for _, out := range m.Call(args) {
			if e, ok := out.Interface().(error); ok && e != nil {
				err = e
			}
		}
	}()
	return err, panicked
}

func without(all []string, drop string) []string {
	out := make([]string, 0, len(all))
	for _, p := range all {
		if p != drop {
			out = append(out, p)
		}
	}
	return out
}
