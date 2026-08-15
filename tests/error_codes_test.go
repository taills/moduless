package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	sqlc "github.com/taills/moduless/core/db/sqlc"
	"github.com/taills/moduless/core/hostsvc"
	pb "github.com/taills/moduless/proto/plugin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// What a refusal tells a plugin.
//
// Every capability classifies its failures into gRPC codes so a plugin can
// branch on them — except two that did not, in different ways.
//
// Fetch returned its backend error raw, so grpc-go wrapped everything in
// codes.Unknown: a host that was never granted, a rate limit, and a remote
// server that is down all arrived identical. The first is permanent and means
// editing the manifest; the second means waiting; the third means retrying.
//
// The file capability had the opposite problem: one condition, mapped three
// ways by its three callers. A missing file was NotFound from DeleteFile and
// Internal from the other two — so a plugin could not branch on it, and an
// operator watching for Internal was paged by an ordinary missing file.
//
// These tests are about the code, not the message. A plugin reads the code.

func codeOf(t *testing.T, err error) codes.Code {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal, got success")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error is not a gRPC status: %v", err)
	}
	return st.Code()
}

// egressServer builds a host service whose plugin may reach exactly one host.
func egressServer(t *testing.T, allow []string, rate int) *hostsvc.Server {
	t.Helper()

	eg := hostsvc.NewHTTPEgress(func(string) []string { return allow })
	if rate > 0 {
		eg.RatePerMinute = rate
	}
	return hostsvc.New("fetcher", []string{"http:egress"}, hostsvc.Deps{Egress: eg})
}

func fetch(t *testing.T, s *hostsvc.Server, url string) error {
	t.Helper()
	_, err := s.Fetch(context.Background(), &pb.FetchRequest{Method: http.MethodGet, Url: url})
	return err
}

// The four outbound failures a plugin author actually meets, each with its own
// code. Table-driven because the point is that they differ from one another,
// not that any one of them has a particular value.
func TestOutboundFailuresAreDistinguishable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	seen := map[string]codes.Code{}

	t.Run("host not in egress_allow", func(t *testing.T) {
		s := egressServer(t, []string{"allowed.example.com"}, 0)
		got := codeOf(t, fetch(t, s, "https://forbidden.example.com/x"))
		if got != codes.PermissionDenied {
			t.Errorf("code = %s, want PermissionDenied; this is permanent and means "+
				"editing the manifest, not retrying", got)
		}
		seen["not allowed"] = got
	})

	t.Run("malformed url", func(t *testing.T) {
		s := egressServer(t, []string{"allowed.example.com"}, 0)
		got := codeOf(t, fetch(t, s, "ftp://allowed.example.com/x"))
		if got != codes.InvalidArgument {
			t.Errorf("code = %s, want InvalidArgument; the plugin built this URL wrongly", got)
		}
		seen["bad request"] = got
	})

	t.Run("rate limited", func(t *testing.T) {
		// One request per minute, and the first one spends it. That first
		// request does not succeed — the address guard refuses loopback — but
		// the rate check runs before the dial, which is what makes this the
		// second request's answer rather than the guard's.
		s := egressServer(t, []string{"127.0.0.1"}, 1)
		_ = fetch(t, s, upstream.URL)

		got := codeOf(t, fetch(t, s, upstream.URL))
		if got != codes.ResourceExhausted {
			t.Errorf("code = %s, want ResourceExhausted; this one clears by waiting", got)
		}
		seen["rate limited"] = got
	})

	t.Run("upstream unreachable", func(t *testing.T) {
		// .invalid never resolves, by RFC 2606, so this is a transport failure
		// that needs no network and cannot flake. A local server would not do:
		// the address guard refuses loopback, so the request would never reach
		// the dial and the failure under test would not be the one produced.
		s := egressServer(t, []string{"no-such-host.invalid"}, 0)
		got := codeOf(t, fetch(t, s, "https://no-such-host.invalid/x"))
		if got == codes.Unknown {
			t.Errorf("code = Unknown; a plugin cannot tell this from any other failure")
		}
		if got != codes.Unavailable {
			t.Errorf("code = %s, want Unavailable; the remote host is unreachable, which is retryable", got)
		}
		seen["unreachable"] = got
	})

	// And they are actually different from each other, which is the property a
	// plugin depends on. Four failures all correctly classified as the same
	// code would pass every check above.
	distinct := map[codes.Code]string{}
	for name, code := range seen {
		if other, clash := distinct[code]; clash {
			t.Errorf("%q and %q both return %s; a plugin cannot tell them apart", name, other, code)
		}
		distinct[code] = name
	}
}

// A plugin without the egress grant is refused for the permission, before any
// of the above is reached. Both are PermissionDenied, so the message has to
// carry the difference — one is fixed in the manifest's permissions list and
// the other in its egress_allow list.
func TestEgressPermissionAndAllowListSayWhichIsMissing(t *testing.T) {
	noGrant := hostsvc.New("fetcher", nil, hostsvc.Deps{
		Egress: hostsvc.NewHTTPEgress(func(string) []string { return []string{"anywhere.example.com"} }),
	})
	permErr := fetch(t, noGrant, "https://anywhere.example.com/x")
	if codeOf(t, permErr) != codes.PermissionDenied {
		t.Fatalf("a missing permission was not PermissionDenied: %v", permErr)
	}

	granted := egressServer(t, []string{"allowed.example.com"}, 0)
	allowErr := fetch(t, granted, "https://forbidden.example.com/x")

	if permErr.Error() == allowErr.Error() {
		t.Fatal("a missing permission and a host outside egress_allow are indistinguishable")
	}
	t.Logf("permission: %v", permErr)
	t.Logf("allow-list: %v", allowErr)
}

// Acting on a file that is not there is NotFound, from every RPC that acts on
// one. GenerateDownloadToken used to report it as Internal, which is a server
// fault — so an operator watching for those was paged by an ordinary 404, and
// a plugin had no way to tell "this id is gone" from "Core is broken".
//
// GetFileMetadata is deliberately not in this list. It asks whether a file
// exists rather than acting on one, and the proto gives it a Found field to
// answer with; that is checked separately below.
func TestMissingFileIsNotFoundEverywhere(t *testing.T) {
	handle := requireDB(t)
	files := hostsvc.NewFiles(handle, sqlc.New(handle), nil)
	s := hostsvc.New("filer", []string{"files:read", "files:write"},
		hostsvc.Deps{Files: files})
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"DeleteFile", func() error {
			_, err := s.DeleteFile(ctx, &pb.FileRequest{FileId: "no-such-file"})
			return err
		}},
		{"GenerateDownloadToken", func() error {
			_, err := s.GenerateDownloadToken(ctx, &pb.DownloadTokenRequest{
				FileId: "no-such-file", ExpirySeconds: 60})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := codeOf(t, tc.call())
			if got == codes.Internal {
				t.Errorf("a missing file is reported as Internal; an operator watching "+
					"for server faults gets paged by an ordinary 404 (%s)", tc.name)
			}
			if got != codes.NotFound {
				t.Errorf("code = %s, want NotFound", got)
			}
		})
	}
}

// Asking about a file that is not there is an answer, not a failure.
//
// GetFileMetadata is the exists-check — the response carries a Found field for
// exactly this — so a plugin can ask without having to treat the ordinary case
// as an error. What it must not do is leak: a file that exists but belongs to
// another plugin has to look identical to one that does not exist, or the
// call becomes a way to enumerate other plugins' file ids.
func TestFileMetadataReportsAbsenceWithoutAnError(t *testing.T) {
	handle := requireDB(t)
	files := hostsvc.NewFiles(handle, sqlc.New(handle), nil)
	s := hostsvc.New("filer", []string{"files:read"}, hostsvc.Deps{Files: files})

	meta, err := s.GetFileMetadata(context.Background(), &pb.FileRequest{FileId: "no-such-file"})
	if err != nil {
		t.Fatalf("asking about a missing file failed instead of answering: %v", err)
	}
	if meta.GetFound() {
		t.Error("a file that does not exist was reported as found")
	}
}
