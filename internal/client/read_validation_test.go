package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func hardenedReadFixture(t *testing.T, path string) (string, string, map[string]any) {
	t.Helper()
	identity := map[string]string{"/project.one": "projectId", "/environment.one": "environmentId", "/compose.one": "composeId", "/domain.one": "domainId", "/destination.one": "destinationId", "/server.one": "serverId", "/sshKey.one": "sshKeyId", "/volumeBackups.one": "volumeBackupId"}[path]
	if identity == "" {
		t.Fatal("unhandled fixture")
	}
	return identity, "expected", map[string]any{identity: "expected"}
}

func TestHardenedReadRejectsUntrustedSuccess(t *testing.T) {
	paths := []string{"/compose.one", "/project.one", "/environment.one", "/domain.one", "/destination.one", "/server.one", "/sshKey.one", "/volumeBackups.one"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			key, id, object := hardenedReadFixture(t, path)
			valid, _ := json.Marshal(object)
			wrong := map[string]any{}
			for k, v := range object {
				wrong[k] = v
			}
			wrong[key] = "foreign"
			wrongRaw, _ := json.Marshal(wrong)
			duplicates := append([]byte(`{"`+key+`":"foreign",`), valid[1:]...)
			folded := append([]byte(`{"`+strings.ToUpper(key)+`":"foreign",`), valid[1:]...)
			for name, body := range map[string][]byte{"empty": {}, "null": []byte(`null`), "object": []byte(`{}`), "array": []byte(`[]`), "wrong_id": wrongRaw, "duplicate_id": duplicates, "case_variant_id": folded, "malformed": []byte(`{"` + key + `":`), "trailing": append(append([]byte{}, valid...), []byte(` {}`)...)} {
				t.Run(name, func(t *testing.T) {
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
					defer srv.Close()
					out := map[string]any{"prior": "state"}
					before := map[string]any{"prior": "state"}
					err := testClient(t, srv).Get(context.Background(), path, url.Values{key: {id}}, &out)
					if err == nil || errors.Is(err, ErrNotFound) {
						t.Fatalf("invalid success err=%v", err)
					}
					if !reflect.DeepEqual(out, before) {
						t.Fatalf("failed read modified caller: %+v", out)
					}
				})
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(valid) }))
			defer srv.Close()
			var out map[string]any
			if err := testClient(t, srv).Get(context.Background(), path, url.Values{key: {id}}, &out); err != nil {
				t.Fatalf("valid identity object: %v", err)
			}
		})
	}
}

func TestHardenedAmbiguous404DoesNotEraseState(t *testing.T) {
	for name, body := range map[string]string{
		"empty": "", "html": "<html>404 Not Found</html>", "malformed": `{"message":`, "unstructured": `{"message":"Project not found","code":"NOT_FOUND"}`,
		"wrong_code":          `{"code":"UNAUTHORIZED","message":"Project not found","data":{"code":"NOT_FOUND","httpStatus":404,"path":"project.one"}}`,
		"wrong_data_code":     `{"code":"NOT_FOUND","message":"Project not found","data":{"code":"FORBIDDEN","httpStatus":404,"path":"project.one"}}`,
		"nested_case_variant": `{"code":"NOT_FOUND","message":"Project not found","data":{"code":"FORBIDDEN","Code":"NOT_FOUND","httpStatus":404,"path":"project.one"}}`,
		"wrong_http_status":   `{"code":"NOT_FOUND","message":"Project not found","data":{"code":"NOT_FOUND","httpStatus":403,"path":"project.one"}}`,
		"wrong_route":         `{"code":"NOT_FOUND","message":"Project not found","data":{"code":"NOT_FOUND","httpStatus":404,"path":"compose.one"}}`,
		"duplicate_code":      `{"code":"UNAUTHORIZED","code":"NOT_FOUND","message":"Project not found","data":{"code":"NOT_FOUND","httpStatus":404,"path":"project.one"}}`,
		"duplicate_nested":    `{"code":"NOT_FOUND","message":"Project not found","data":{"code":"NOT_FOUND","httpStatus":403,"httpStatus":404,"path":"project.one"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404); _, _ = fmt.Fprint(w, body) }))
			defer srv.Close()
			err := testClient(t, srv).Get(context.Background(), "/project.one", url.Values{"projectId": {"gone"}}, new(Project))
			if err == nil || errors.Is(err, ErrNotFound) {
				t.Fatalf("ambiguous404 err=%v", err)
			}
		})
	}
	for _, status := range []int{401, 403, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, `{"code":"NOT_FOUND","message":"Project not found","data":{"code":"NOT_FOUND","httpStatus":404,"path":"project.one"}}`)
			}))
			defer srv.Close()
			err := testClient(t, srv).Get(context.Background(), "/project.one", url.Values{"projectId": {"gone"}}, new(Project))
			if err == nil || errors.Is(err, ErrNotFound) {
				t.Fatalf("status%d err=%v", status, err)
			}
		})
	}
}

// These protocol mocks deliberately use the independently recorded diagnostic
// strings. Their data blocks exercise the tRPC transport contract separately
// from the original recorded message/code fixtures.
func TestHardenedConfirmedMissingRecord(t *testing.T) {
	for _, test := range []struct {
		path, key, message, code string
		status                   int
	}{
		{"/ai.one", "aiId", "AI settings not found", "NOT_FOUND", 404},
		{"/bitbucket.one", "bitbucketId", "Bitbucket Provider not found", "NOT_FOUND", 404},
		{"/gitea.one", "giteaId", "Gitea Provider not found", "NOT_FOUND", 404},
		{"/gitlab.one", "gitlabId", "Gitlab Provider not found", "NOT_FOUND", 404},
		{"/user.one", "userId", "User not found in this organization", "NOT_FOUND", 404},
		{"/port.one", "portId", "Port not found", "BAD_REQUEST", 400},
	} {
		t.Run(test.path, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"code": test.code, "message": test.message, "data": map[string]any{"code": test.code, "httpStatus": test.status, "path": strings.TrimPrefix(test.path, "/")}})
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(test.status); _, _ = w.Write(raw) }))
			defer srv.Close()
			err = testClient(t, srv).Get(context.Background(), test.path, url.Values{test.key: {"gone"}}, nil)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("confirmed missing record: %v", err)
			}
		})
	}
}

func TestHardenedResponseLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(w, io.LimitReader(strings.NewReader(strings.Repeat("x", maxResponseBytes+1)), maxResponseBytes+1))
	}))
	defer srv.Close()
	err := testClient(t, srv).Get(context.Background(), "/project.all", nil, new([]Project))
	if err == nil {
		t.Fatal("oversized response accepted")
	}
}

func TestHardenedMutationRejectsHTMLSuccess(t *testing.T) {
	for _, route := range []string{"/compose.delete", "/project.remove", "/compose.update"} {
		t.Run(route, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = fmt.Fprint(w, "<html>unexpected login page</html>")
			}))
			defer srv.Close()
			err := testClient(t, srv).Post(context.Background(), route, map[string]string{"composeId": "owned"}, nil)
			if err == nil || calls.Load() != 1 {
				t.Fatalf("mutation err=%v attempts=%d", err, calls.Load())
			}
		})
	}
}

func TestReadDecodeFailurePreservesOutput(t *testing.T) {
	out := map[string]int{"prior": 7}
	if err := decodeResponse([]byte(`{"prior":"wrong type"}`), &out); err == nil || out["prior"] != 7 {
		t.Fatalf("decode failed to preserve prior output: %+v err=%v", out, err)
	}
}

func TestReadAllowsSparseObjectsAndFutureValues(t *testing.T) {
	for name, body := range map[string]string{
		"identity_only": `{"composeId":"expected"}`,
		"future_fields": `{"composeId":"expected","composeType":"future-compose","sourceType":"future-source","composeStatus":"future-status","futureField":{"enabled":true},"serviceNetworks":[{"futureNetworkField":true}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, body) }))
			defer srv.Close()
			var out Compose
			if err := testClient(t, srv).Get(context.Background(), "/compose.one", url.Values{"composeId": {"expected"}}, &out); err != nil || out.ComposeID != "expected" {
				t.Fatalf("read=%+v err=%v", out, err)
			}
		})
	}
}

func TestJSONNestedMapKeyCase(t *testing.T) {
	for _, test := range []struct {
		name, body string
		valid      bool
	}{
		{"docker_labels", `{"composeId":"expected","labels":{"Team":"Ops","team":"Dev"}}`, true},
		{"swarm_labels", `{"composeId":"expected","swarm":{"labels":{"Zone":"EU","zone":"US"}}}`, true},
		{"nested_array", `{"items":[{"Team":"Ops","team":"Dev"}]}`, true},
		{"duplicate_nested", `{"labels":{"team":"Ops","team":"Dev"}}`, false},
		{"duplicate_in_array", `{"items":[{"team":"Ops","team":"Dev"}]}`, false},
		{"duplicate_top_level", `{"composeId":"expected","composeId":"foreign"}`, false},
		{"folded_top_level", `{"composeId":"expected","ComposeId":"foreign"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := strictJSON([]byte(test.body)); (err == nil) != test.valid {
				t.Fatalf("valid=%v err=%v", test.valid, err)
			}
		})
	}
}

func TestDeleteAPIKeyAlreadyGone(t *testing.T) {
	for _, test := range []struct {
		name, path, body string
		gone             bool
	}{
		{"known_delete", "/user.deleteApiKey", `{"message":"API key not found","code":"NOT_FOUND","data":{"code":"NOT_FOUND","httpStatus":404,"path":"user.deleteApiKey"}}`, true},
		{"wrong_route", "/user.deleteApiKey", `{"message":"API key not found","code":"NOT_FOUND","data":{"code":"NOT_FOUND","httpStatus":404,"path":"user.delete"}}`, false},
		{"proxy_html", "/user.deleteApiKey", `<html>404 Not Found</html>`, false},
		{"unrelated_post", "/user.update", `{"message":"User not found","code":"NOT_FOUND","data":{"code":"NOT_FOUND","httpStatus":404,"path":"user.update"}}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404); _, _ = fmt.Fprint(w, test.body) }))
			defer srv.Close()
			c := testClient(t, srv)
			var err error
			if test.path == "/user.deleteApiKey" {
				err = c.DeleteAPIKey(context.Background(), "gone")
			} else {
				err = c.Post(context.Background(), test.path, map[string]string{"userId": "gone"}, nil)
			}
			if err == nil || errors.Is(err, ErrNotFound) != test.gone {
				t.Fatalf("gone=%v err=%v", test.gone, err)
			}
		})
	}
}

func TestOrganizationForbiddenMappingIsExact(t *testing.T) {
	for _, test := range []struct {
		message, code string
		gone          bool
	}{
		{"You are not a member of this organization", "FORBIDDEN", true},
		{"Forbidden", "FORBIDDEN", false},
		{"You are not a member of this organization anymore", "FORBIDDEN", false},
		{"You are not a member of this organization", "UNAUTHORIZED", false},
	} {
		t.Run(test.message+test.code, func(t *testing.T) {
			raw, err := json.Marshal(map[string]string{"message": test.message, "code": test.code})
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403); _, _ = w.Write(raw) }))
			defer srv.Close()
			_, err = testClient(t, srv).GetOrganization(context.Background(), "gone")
			if err == nil || errors.Is(err, ErrNotFound) != test.gone {
				t.Fatalf("gone=%v err=%v", test.gone, err)
			}
		})
	}
}

func TestDecodeTypeErrorDoesNotPartiallyOverwriteStruct(t *testing.T) {
	out := Project{ProjectID: "prior", Name: "keep"}
	before := out
	if err := decodeResponse([]byte(`{"name":"overwritten","projectId":123}`), &out); err == nil || !reflect.DeepEqual(out, before) {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}
