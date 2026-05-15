package externalfs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	"github.com/opencloud-eu/reva/v2/pkg/storage"
)

func TestExternalFSDiscoversAllowlistedSpacesAndReadsWithoutWriting(t *testing.T) {
	root := t.TempDir()
	photos := filepath.Join(root, "photos")
	private := filepath.Join(root, "private")
	if err := os.Mkdir(photos, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(private, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(photos, "image.jpg"), []byte("original-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "secret.txt"), []byte("not-adopted"), 0o644); err != nil {
		t.Fatal(err)
	}

	before := snapshotTree(t, root)
	fs := newTestFS(t, map[string]interface{}{
		"mount_id": "external-mount",
		"datasources": []map[string]interface{}{
			{
				"id":         "media",
				"root":       root,
				"mount_name": "Archive",
				"adopt":      []string{"photos"},
				"read_only":  true,
			},
		},
	})

	spaces, err := fs.ListStorageSpaces(context.Background(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(spaces) != 1 {
		t.Fatalf("expected one adopted space, got %d", len(spaces))
	}
	if spaces[0].GetName() != "Archive - photos" {
		t.Fatalf("unexpected space name %q", spaces[0].GetName())
	}
	spacesByID, err := fs.ListStorageSpaces(context.Background(), []*provider.ListStorageSpacesRequest_Filter{
		{
			Type: provider.ListStorageSpacesRequest_Filter_TYPE_ID,
			Term: &provider.ListStorageSpacesRequest_Filter_Id{
				Id: &provider.StorageSpaceId{OpaqueId: spaces[0].GetId().GetOpaqueId()},
			},
		},
		{
			Type: provider.ListStorageSpacesRequest_Filter_TYPE_SPACE_TYPE,
			Term: &provider.ListStorageSpacesRequest_Filter_SpaceType{
				SpaceType: "+grant",
			},
		},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(spacesByID) != 1 {
		t.Fatalf("expected internal +grant lookup to preserve external space, got %d", len(spacesByID))
	}

	rootRef := &provider.Reference{ResourceId: spaces[0].GetRoot()}
	children, err := fs.ListFolder(context.Background(), rootRef, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].GetName() != "image.jpg" {
		t.Fatalf("expected image.jpg child, got %#v", children)
	}

	fileRef := &provider.Reference{ResourceId: spaces[0].GetRoot(), Path: "image.jpg"}
	ri, rc, err := fs.Download(context.Background(), fileRef, func(*provider.ResourceInfo) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if ri.GetName() != "image.jpg" || string(body) != "original-bytes" {
		t.Fatalf("unexpected download %q %q", ri.GetName(), string(body))
	}

	if err := fs.CreateDir(context.Background(), &provider.Reference{ResourceId: spaces[0].GetRoot(), Path: "new"}); err == nil {
		t.Fatal("CreateDir unexpectedly succeeded")
	}
	if err := fs.Delete(context.Background(), fileRef); err == nil {
		t.Fatal("Delete unexpectedly succeeded")
	}
	if _, err := fs.InitiateUpload(context.Background(), fileRef, 10, nil); err == nil {
		t.Fatal("InitiateUpload unexpectedly succeeded")
	}

	after := snapshotTree(t, root)
	if before != after {
		t.Fatalf("source tree changed\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestExternalFSWritesWhenDatasourceIsWritable(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, map[string]interface{}{
		"mount_id": "external-mount",
		"datasources": []map[string]interface{}{
			{
				"id":         "music",
				"root":       root,
				"mount_name": "Music",
				"adopt":      []string{"."},
			},
		},
	})
	spaces, err := fs.ListStorageSpaces(context.Background(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(spaces) != 1 {
		t.Fatalf("expected one space, got %d", len(spaces))
	}
	perms := spaces[0].GetPermissionSet()
	if !perms.GetCreateContainer() || !perms.GetInitiateFileUpload() || perms.GetDelete() {
		t.Fatalf("unexpected permissions: %#v", perms)
	}

	rootRef := &provider.Reference{ResourceId: spaces[0].GetRoot()}
	if err := fs.CreateDir(context.Background(), &provider.Reference{ResourceId: spaces[0].GetRoot(), Path: "new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Upload(context.Background(), storageUpload(rootRef, "new/track.flac", "bytes"), nil); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(root, "new", "track.flac"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "bytes" {
		t.Fatalf("unexpected written body %q", string(body))
	}
	if err := fs.Delete(context.Background(), &provider.Reference{ResourceId: spaces[0].GetRoot(), Path: "new/track.flac"}); err == nil {
		t.Fatal("Delete unexpectedly succeeded without allow_deletes")
	}
}

func TestExternalFSRejectsEscapingPaths(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "source")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "space"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "outside.txt"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := newTestFS(t, map[string]interface{}{
		"mount_id": "external-mount",
		"datasources": []map[string]interface{}{
			{
				"id":    "source",
				"root":  root,
				"adopt": []string{"space"},
			},
		},
	})
	spaces, err := fs.ListStorageSpaces(context.Background(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(spaces) != 1 {
		t.Fatalf("expected one space, got %d", len(spaces))
	}

	_, err = fs.GetMD(context.Background(), &provider.Reference{ResourceId: spaces[0].GetRoot(), Path: "../outside.txt"}, nil, nil)
	if err == nil {
		t.Fatal("path traversal unexpectedly succeeded")
	}
}

func storageUpload(rootRef *provider.Reference, rel, body string) storage.UploadRequest {
	return storage.UploadRequest{
		Ref: &provider.Reference{
			ResourceId: rootRef.GetResourceId(),
			Path:       rel,
		},
		Body:   io.NopCloser(strings.NewReader(body)),
		Length: int64(len(body)),
	}
}

func TestExternalFSCanExposeDatasourceRootAsSpace(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "track.flac"), []byte("music"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := newTestFS(t, map[string]interface{}{
		"mount_id": "external-mount",
		"datasources": []map[string]interface{}{
			{
				"id":         "music",
				"root":       root,
				"mount_name": "Music",
				"adopt":      []string{"."},
			},
		},
	})
	spaces, err := fs.ListStorageSpaces(context.Background(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(spaces) != 1 || spaces[0].GetName() != "Music" {
		t.Fatalf("expected Music root space, got %#v", spaces)
	}
	children, err := fs.ListFolder(context.Background(), &provider.Reference{ResourceId: spaces[0].GetRoot()}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].GetName() != "track.flac" {
		t.Fatalf("expected root file in listed children, got %#v", children)
	}
}

func newTestFS(t *testing.T, cfg map[string]interface{}) *externalFS {
	t.Helper()
	driver, err := New(cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fs, ok := driver.(*externalFS)
	if !ok {
		t.Fatalf("unexpected fs type %T", driver)
	}
	return fs
}

func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var out string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out += rel + "|" + info.Mode().String() + "\n"
		if !d.IsDir() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out += string(b) + "\n"
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
