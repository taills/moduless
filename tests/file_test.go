package tests

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/taills/moduless/core/db/sqlc"
	"github.com/taills/moduless/core/gateway"
	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/core/storage"
	pb "github.com/taills/moduless/proto/plugin"
)

// Files, end to end, across the asymmetry that defines them.
//
// Writes go out through the plugin transport in chunks. Reads do not come back
// through it at all: the plugin asks for a short-lived URL and the browser
// fetches from Core directly. That asymmetry is the design — a report or an
// attachment would otherwise be buffered twice and cross a process boundary
// for no reason — and it is only observable end to end, because it is a claim
// about which component does not see the bytes.
//
//	TEST_S3_ENDPOINT=http://localhost:19000 TEST_DATABASE_URL=... go test ./tests/ -run File

func requireS3(t *testing.T) (*storage.RustFSClient, string) {
	t.Helper()

	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_S3_ENDPOINT is not set")
	}
	accessKey := envOr("TEST_S3_ACCESS_KEY", "moduless")
	secretKey := envOr("TEST_S3_SECRET_KEY", "moduless123")
	bucket := envOr("TEST_S3_BUCKET", "moduless-test")

	// Create the bucket if it is not there, so a fresh object store needs no
	// manual setup before the suite runs.
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		t.Fatalf("s3 config: %v", err)
	}
	raw := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	if _, err := raw.CreateBucket(context.Background(), &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	}); err != nil && !strings.Contains(err.Error(), "BucketAlreadyOwnedByYou") &&
		!strings.Contains(err.Error(), "BucketAlreadyExists") {
		t.Skipf("object store unreachable: %v", err)
	}

	client, err := storage.NewRustFSClient(endpoint, bucket, accessKey, secretKey)
	if err != nil {
		t.Fatalf("building the storage client: %v", err)
	}
	return client, bucket
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// fileStack wires a plugin, the file service and Core's download route, which
// is the whole path a generated report travels.
func fileStack(t *testing.T, key string, granted []string) (inst *pluginhost.Instance, coreURL string) {
	t.Helper()

	handle := requireDB(t)
	store, _ := requireS3(t)

	files := hostsvc.NewFiles(handle, sqlc.New(handle), store)

	inst, err := pluginhost.Launch(context.Background(), pluginhost.LaunchSpec{
		Key:        key,
		InstanceID: key + "-0",
		Version:    "1.0.0",
		BinaryPath: pluginBinary,
		Checksum:   checksum(t, pluginBinary),
		HostImpl: hostsvc.New(key, granted, hostsvc.Deps{
			Config: hostsvc.NewStaticConfig(),
			Files:  files,
		}),
		GrantedPermissions: granted,
		Env:                []string{"PATH=/usr/bin:/bin"},
		Stderr:             os.Stderr,
		DevMode:            true,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(inst.Kill)

	// Core's own download route, which is what the browser talks to. Nothing
	// about the plugin is involved in serving it.
	h := gateway.NewFileHandler(store, sqlc.New(handle))
	mux := http.NewServeMux()
	mux.HandleFunc(downloadPrefix, h.Download)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return inst, srv.URL
}

// downloadPrefix is where Core serves file downloads, as main.go registers it.
const downloadPrefix = "/api/system/files/download/"

// uploadedFile is what the fixture reports back.
type uploadedFile struct {
	id   string
	size int64
	url  string
}

func uploadVia(t *testing.T, inst *pluginhost.Instance, content string) uploadedFile {
	t.Helper()

	resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet,
		Path:   "/file",
		Query:  content,
	})
	if err != nil {
		t.Fatalf("calling the plugin: %v", err)
	}
	if resp.GetStatusCode() != 200 {
		t.Fatalf("upload failed: %s", resp.GetBody())
	}

	var out uploadedFile
	body := string(resp.GetBody())
	for _, field := range strings.Fields(body) {
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch k {
		case "id":
			out.id = v
		case "url":
			out.url = v
		case "size":
			_, _ = fmt.Sscanf(v, "%d", &out.size)
		}
	}
	if out.id == "" || out.url == "" {
		t.Fatalf("could not parse the plugin's reply: %q", body)
	}
	return out
}

// The headline path: a plugin writes a file through Core, gets a link, and the
// bytes come back from Core over ordinary HTTP without the plugin involved.
func TestFileUploadAndBrowserDownload(t *testing.T) {
	inst, coreURL := fileStack(t, "filer", []string{"files:write", "files:read"})

	const content = "generated report"
	file := uploadVia(t, inst, content)
	t.Logf("uploaded %s (%d bytes), url %s", file.id, file.size, file.url)

	if file.size == 0 {
		t.Error("the upload reported zero bytes")
	}

	// The plugin is now out of the picture entirely — this is a plain browser
	// fetching from Core.
	resp, err := http.Get(coreURL + file.url)
	if err != nil {
		t.Fatalf("downloading: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		t.Fatalf("download returned %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), content) {
		t.Errorf("downloaded %q, want it to contain what was uploaded", body)
	}
	// The second chunk proves the streaming upload was reassembled in order
	// rather than only the first message being kept.
	if !strings.Contains(string(body), "trailing chunk") {
		t.Error("the second uploaded chunk is missing from the stored file")
	}
}

// Writing a file needs files:write, like every other capability.
func TestFileUploadRequiresPermission(t *testing.T) {
	inst, _ := fileStack(t, "filer", nil)

	resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet, Path: "/file", Query: "no permission",
	})
	if err != nil {
		t.Fatalf("calling the plugin: %v", err)
	}
	if resp.GetStatusCode() == 200 {
		t.Fatal("a plugin wrote a file without the files:write permission")
	}
	if body := string(resp.GetBody()); !strings.Contains(body, "files:write") {
		t.Errorf("the refusal does not name the missing permission: %q", body)
	}
}

// A download link is a bearer token in a URL. It must be worthless without the
// token, or the file id alone — which appears in logs and in a plugin's own
// records — would be enough to read anyone's file.
func TestFileDownloadRequiresTheToken(t *testing.T) {
	inst, coreURL := fileStack(t, "filer", []string{"files:write", "files:read"})

	file := uploadVia(t, inst, "secret contents")

	// Strip the token, keep the id.
	trimmed := strings.TrimSuffix(file.url, "/"+lastSegment(file.url))

	resp, err := http.Get(coreURL + trimmed)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	t.Logf("download without a token: %d", resp.StatusCode)
	if resp.StatusCode == 200 {
		t.Error("a file was served without its download token")
	}
	if strings.Contains(string(body), "secret contents") {
		t.Error("the file contents came back without a token")
	}
}

// A wrong token must be refused as firmly as a missing one.
func TestFileDownloadRejectsAWrongToken(t *testing.T) {
	inst, coreURL := fileStack(t, "filer", []string{"files:write", "files:read"})

	file := uploadVia(t, inst, "secret contents")
	tampered := strings.TrimSuffix(file.url, lastSegment(file.url)) + "not-the-token"

	resp, err := http.Get(coreURL + tampered)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == 200 {
		t.Error("a forged download token was accepted")
	}
	if strings.Contains(string(body), "secret contents") {
		t.Error("the file contents came back for a forged token")
	}
}

// The download URL must carry its token as a path segment, not a query string.
// Query strings end up in access logs, proxy logs and Referer headers; a path
// parameter is not much better in principle but is what the rest of this
// system already assumes, and mixing the two would break the download route.
func TestFileDownloadURLShape(t *testing.T) {
	inst, _ := fileStack(t, "filer", []string{"files:write", "files:read"})

	file := uploadVia(t, inst, "anything")
	t.Logf("download url: %s", file.url)

	if strings.Contains(file.url, "?") {
		t.Errorf("the download URL carries a query string: %s", file.url)
	}
	if !strings.HasPrefix(file.url, downloadPrefix) {
		t.Errorf("the download URL %q does not start with %q", file.url, downloadPrefix)
	}
}

func lastSegment(path string) string {
	i := strings.LastIndexByte(path, '/')
	if i < 0 {
		return path
	}
	return path[i+1:]
}

// A download link must stop working when it says it will. The expiry is the
// only thing limiting how long a leaked URL — in a chat message, a log, a
// browser history — remains a working key to the file.
func TestFileDownloadTokenExpires(t *testing.T) {
	inst, coreURL := fileStack(t, "filer", []string{"files:write", "files:read"})

	resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet, Path: "/file-shortlived", Query: "expiring contents",
	})
	if err != nil {
		t.Fatalf("calling the plugin: %v", err)
	}
	if resp.GetStatusCode() != 200 {
		t.Fatalf("upload failed: %s", resp.GetBody())
	}
	url := strings.TrimPrefix(strings.Fields(string(resp.GetBody()))[2], "url=")

	// It works right away.
	first, err := http.Get(coreURL + url)
	if err != nil {
		t.Fatalf("first download: %v", err)
	}
	io.Copy(io.Discard, first.Body)
	first.Body.Close()
	if first.StatusCode != 200 {
		t.Fatalf("a fresh token was refused: %d", first.StatusCode)
	}

	// And stops working once it has expired.
	time.Sleep(2500 * time.Millisecond)

	second, err := http.Get(coreURL + url)
	if err != nil {
		t.Fatalf("second download: %v", err)
	}
	defer second.Body.Close()
	body, _ := io.ReadAll(second.Body)

	t.Logf("after expiry: %d", second.StatusCode)
	if second.StatusCode == 200 {
		t.Error("an expired download token still works")
	}
	if strings.Contains(string(body), "expiring contents") {
		t.Error("the file contents came back after the token expired")
	}
}

// One plugin must not be able to mint a download link for another plugin's
// file. File ids travel — in logs, in events, in a plugin's own records — so
// holding one cannot be what grants access.
func TestFileIsolatedBetweenPlugins(t *testing.T) {
	owner, coreURL := fileStack(t, "owner", []string{"files:write", "files:read"})
	file := uploadVia(t, owner, "owner's private report")

	// A second plugin, with the same permissions, holding the file id.
	other, _ := fileStack(t, "intruder", []string{"files:write", "files:read"})

	resp, err := other.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet, Path: "/file-token-for", Query: file.id,
	})
	if err != nil {
		t.Fatalf("calling the plugin: %v", err)
	}

	body := string(resp.GetBody())
	t.Logf("another plugin asking for a token: %d %s", resp.GetStatusCode(), body)

	if resp.GetStatusCode() != 200 {
		return // refused outright, which is the strongest answer
	}

	// If a token was issued at all, it must not actually serve the file.
	url := strings.TrimPrefix(strings.Fields(body)[0], "url=")
	download, err := http.Get(coreURL + url)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer download.Body.Close()
	content, _ := io.ReadAll(download.Body)

	if download.StatusCode == 200 && strings.Contains(string(content), "owner's private report") {
		t.Error("a plugin read another plugin's file by presenting its id")
	}
}

// Deleting is the destructive half of the same mistake: if holding an id is
// enough to read a file, it is enough to destroy one.
func TestFileDeleteIsIsolatedBetweenPlugins(t *testing.T) {
	owner, coreURL := fileStack(t, "owner", []string{"files:write", "files:read"})
	file := uploadVia(t, owner, "do not delete me")

	other, _ := fileStack(t, "intruder", []string{"files:write", "files:read"})

	resp, err := other.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet, Path: "/file-delete", Query: file.id,
	})
	if err != nil {
		t.Fatalf("calling the plugin: %v", err)
	}
	t.Logf("another plugin deleting: %d %s", resp.GetStatusCode(), resp.GetBody())

	if resp.GetStatusCode() == 200 {
		t.Error("a plugin deleted another plugin's file")
	}

	// The owner's file is still there and still downloadable.
	download, err := http.Get(coreURL + file.url)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer download.Body.Close()
	body, _ := io.ReadAll(download.Body)
	if download.StatusCode != 200 || !strings.Contains(string(body), "do not delete me") {
		t.Errorf("the owner's file is gone after another plugin tried to delete it: %d %s",
			download.StatusCode, body)
	}
}

// Metadata leaks less than content but still leaks: a filename says what a
// system does, and a size says how much of it there is.
func TestFileMetadataIsIsolatedBetweenPlugins(t *testing.T) {
	owner, _ := fileStack(t, "owner", []string{"files:write", "files:read"})
	file := uploadVia(t, owner, "contents")

	// The owner can read its own metadata, so a failure below is about
	// ownership rather than the call not working at all.
	ownMeta, err := owner.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet, Path: "/file-meta", Query: file.id,
	})
	if err != nil {
		t.Fatalf("calling the plugin: %v", err)
	}
	if got := string(ownMeta.GetBody()); !strings.Contains(got, "found=true") {
		t.Fatalf("the owner cannot read its own file's metadata: %s", got)
	}

	other, _ := fileStack(t, "intruder", []string{"files:write", "files:read"})
	otherMeta, err := other.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet, Path: "/file-meta", Query: file.id,
	})
	if err != nil {
		t.Fatalf("calling the plugin: %v", err)
	}

	got := string(otherMeta.GetBody())
	t.Logf("another plugin reading metadata: %d %s", otherMeta.GetStatusCode(), got)

	if strings.Contains(got, "found=true") {
		t.Error("a plugin read another plugin's file metadata")
	}
	if strings.Contains(got, "report.txt") {
		t.Error("the filename leaked to a plugin that does not own the file")
	}
}
