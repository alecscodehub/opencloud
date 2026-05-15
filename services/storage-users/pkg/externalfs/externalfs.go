package externalfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	userv1beta1 "github.com/cs3org/go-cs3apis/cs3/identity/user/v1beta1"
	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	types "github.com/cs3org/go-cs3apis/cs3/types/v1beta1"
	"github.com/mitchellh/mapstructure"
	ctxpkg "github.com/opencloud-eu/reva/v2/pkg/ctx"
	"github.com/opencloud-eu/reva/v2/pkg/errtypes"
	"github.com/opencloud-eu/reva/v2/pkg/events"
	"github.com/opencloud-eu/reva/v2/pkg/mime"
	"github.com/opencloud-eu/reva/v2/pkg/rgrpc/status"
	"github.com/opencloud-eu/reva/v2/pkg/storage"
	"github.com/opencloud-eu/reva/v2/pkg/storage/fs/posix"
	"github.com/opencloud-eu/reva/v2/pkg/storage/fs/registry"
	"github.com/opencloud-eu/reva/v2/pkg/storagespace"
	"github.com/opencloud-eu/reva/v2/pkg/utils"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	tusd "github.com/tus/tusd/v2/pkg/handler"
)

func init() {
	registry.Register("external", New)
	registry.Register("posix_external", NewPosixExternal)
}

type config struct {
	MountID     string       `mapstructure:"mount_id"`
	Datasources []datasource `mapstructure:"datasources"`
}

type datasource struct {
	ID           string   `mapstructure:"id"`
	Root         string   `mapstructure:"root"`
	MountName    string   `mapstructure:"mount_name"`
	Adopt        []string `mapstructure:"adopt"`
	OwnerIDP     string   `mapstructure:"owner_idp"`
	OwnerID      string   `mapstructure:"owner_id"`
	ReadOnly     bool     `mapstructure:"read_only"`
	AllowDeletes bool     `mapstructure:"allow_deletes"`
	root         string
}

type externalFS struct {
	mountID string
	spaces  []*space
}

type compositeFS struct {
	primary  storage.FS
	external *externalFS
}

type space struct {
	source      *datasource
	id          string
	name        string
	alias       string
	relativeDir string
	root        string
}

func New(m map[string]interface{}, _ events.Stream, _ *zerolog.Logger) (storage.FS, error) {
	c := &config{}
	if err := mapstructure.Decode(m, c); err != nil {
		return nil, errors.Wrap(err, "externalfs: error decoding config")
	}
	if c.MountID == "" {
		return nil, errtypes.BadRequest("externalfs: mount_id is required")
	}

	fs := &externalFS{mountID: c.MountID}
	seenSources := map[string]string{}
	for i := range c.Datasources {
		ds := c.Datasources[i]
		if ds.ID == "" {
			return nil, errtypes.BadRequest("externalfs: datasource id is required")
		}
		if ds.Root == "" {
			return nil, errtypes.BadRequest("externalfs: datasource root is required")
		}
		root, err := filepath.Abs(ds.Root)
		if err != nil {
			return nil, err
		}
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			return nil, errors.Wrap(err, "externalfs: datasource root must exist")
		}
		info, err := os.Stat(root)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, errtypes.BadRequest("externalfs: datasource root is not a directory")
		}
		for otherID, otherRoot := range seenSources {
			if sameOrChild(root, otherRoot) || sameOrChild(otherRoot, root) {
				return nil, errtypes.BadRequest(fmt.Sprintf("externalfs: datasource roots %q and %q overlap", ds.ID, otherID))
			}
		}
		seenSources[ds.ID] = root
		ds.root = root
		if len(ds.Adopt) == 0 {
			return nil, errtypes.BadRequest("externalfs: datasource adopt allowlist is required")
		}
		discovered, err := discoverSpaces(&ds)
		if err != nil {
			return nil, err
		}
		fs.spaces = append(fs.spaces, discovered...)
	}
	return fs, nil
}

func NewPosixExternal(m map[string]interface{}, stream events.Stream, log *zerolog.Logger) (storage.FS, error) {
	primary, err := posix.NewDefault(nestedMap(m, "posix"), stream, log)
	if err != nil {
		return nil, err
	}
	external, err := New(nestedMap(m, "external"), stream, log)
	if err != nil {
		return nil, err
	}
	efs, ok := external.(*externalFS)
	if !ok {
		return nil, errtypes.BadRequest("externalfs: unexpected external driver type")
	}
	return &compositeFS{primary: primary, external: efs}, nil
}

func nestedMap(m map[string]interface{}, key string) map[string]interface{} {
	if v, ok := m[key].(map[string]interface{}); ok {
		return v
	}
	return map[string]interface{}{}
}

func discoverSpaces(ds *datasource) ([]*space, error) {
	var spaces []*space
	if allowed(".", ds.Adopt) {
		name := filepath.Base(ds.root)
		if ds.MountName != "" {
			name = ds.MountName
		}
		spaces = append(spaces, &space{
			source:      ds,
			id:          stableID(ds.ID, "."),
			name:        name,
			alias:       path.Join("external", ds.ID),
			relativeDir: ".",
			root:        ds.root,
		})
	}
	if onlyRootAdoption(ds.Adopt) {
		return spaces, nil
	}

	entries, err := os.ReadDir(ds.root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !allowed(entry.Name(), ds.Adopt) {
			continue
		}
		root, err := filepath.EvalSymlinks(filepath.Join(ds.root, entry.Name()))
		if err != nil || !sameOrChild(root, ds.root) {
			continue
		}
		name := entry.Name()
		if ds.MountName != "" {
			name = ds.MountName + " - " + entry.Name()
		}
		id := stableID(ds.ID, entry.Name())
		spaces = append(spaces, &space{
			source:      ds,
			id:          id,
			name:        name,
			alias:       path.Join("external", ds.ID, entry.Name()),
			relativeDir: entry.Name(),
			root:        root,
		})
	}
	return spaces, nil
}

func onlyRootAdoption(patterns []string) bool {
	for _, pattern := range patterns {
		if pattern != "." {
			return false
		}
	}
	return len(patterns) > 0
}

func allowed(name string, patterns []string) bool {
	for _, pattern := range patterns {
		if pattern == name {
			return true
		}
		ok, err := filepath.Match(pattern, name)
		if err == nil && ok {
			return true
		}
	}
	return false
}

func stableID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:16])
}

func sameOrChild(candidate, root string) bool {
	candidate = filepath.Clean(candidate)
	root = filepath.Clean(root)
	if strings.EqualFold(candidate, root) {
		return true
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (fs *externalFS) Shutdown(context.Context) error { return nil }

func (fs *externalFS) ListStorageSpaces(ctx context.Context, filters []*provider.ListStorageSpacesRequest_Filter, _ bool) ([]*provider.StorageSpace, error) {
	var requestedID, requestedType string
	var requestedUser *userv1beta1.UserId
	for _, f := range filters {
		switch f.Type {
		case provider.ListStorageSpacesRequest_Filter_TYPE_ID:
			_, requestedID = splitStorageID(f.GetId().GetOpaqueId())
		case provider.ListStorageSpacesRequest_Filter_TYPE_SPACE_TYPE:
			requestedType = f.GetSpaceType()
		case provider.ListStorageSpacesRequest_Filter_TYPE_USER, provider.ListStorageSpacesRequest_Filter_TYPE_OWNER:
			requestedUser = f.GetUser()
			if requestedUser == nil {
				requestedUser = f.GetOwner()
			}
		}
	}

	var result []*provider.StorageSpace
	for _, sp := range fs.spaces {
		if requestedType != "" && requestedType != "project" && !strings.HasPrefix(requestedType, "+") {
			continue
		}
		if requestedID != "" && requestedID != sp.id {
			continue
		}
		owner := sp.owner(ctx)
		if requestedUser != nil && owner != nil && requestedUser.GetOpaqueId() != "" && requestedUser.GetOpaqueId() != owner.GetOpaqueId() {
			continue
		}
		result = append(result, fs.storageSpace(ctx, sp, true))
	}
	return result, nil
}

func splitStorageID(id string) (string, string) {
	parts := strings.Split(id, "$")
	if len(parts) > 1 {
		return parts[0], strings.Split(parts[1], "!")[0]
	}
	return "", strings.Split(id, "!")[0]
}

func (sp *space) owner(c context.Context) *userv1beta1.UserId {
	if sp.source.OwnerID != "" {
		return &userv1beta1.UserId{Idp: sp.source.OwnerIDP, OpaqueId: sp.source.OwnerID}
	}
	if u, ok := ctxpkg.ContextGetUser(c); ok {
		return u.GetId()
	}
	return &userv1beta1.UserId{OpaqueId: "external-datasource", Type: userv1beta1.UserType_USER_TYPE_SPACE_OWNER}
}

func (fs *externalFS) storageSpace(ctx context.Context, sp *space, withRoot bool) *provider.StorageSpace {
	root := &provider.ResourceId{StorageId: fs.mountID, SpaceId: sp.id, OpaqueId: sp.id}
	ss := &provider.StorageSpace{
		Id:        &provider.StorageSpaceId{OpaqueId: fmt.Sprintf("%s$%s", fs.mountID, sp.id)},
		Root:      root,
		Name:      sp.name,
		SpaceType: "project",
		Owner:     &userv1beta1.User{Id: sp.owner(ctx)},
		Opaque:    utils.AppendPlainToOpaque(nil, "spaceAlias", sp.alias),
		PermissionSet: sp.permissions(),
	}
	if withRoot {
		if ri, err := fs.info(ctx, sp, "", true); err == nil {
			ss.RootInfo = ri
			ss.Mtime = ri.Mtime
		}
	}
	return ss
}

func (fs *externalFS) GetQuota(context.Context, *provider.Reference) (uint64, uint64, uint64, error) {
	return 0, 0, 0, nil
}

func (fs *externalFS) GetPathByID(_ context.Context, id *provider.ResourceId) (string, error) {
	if id.GetOpaqueId() == "" || id.GetOpaqueId() == id.GetSpaceId() {
		return ".", nil
	}
	raw, err := url.QueryUnescape(strings.TrimPrefix(id.GetOpaqueId(), "p:"))
	if err != nil {
		return "", err
	}
	return path.Join("/", raw), nil
}

func (fs *externalFS) GetMD(ctx context.Context, ref *provider.Reference, _, _ []string) (*provider.ResourceInfo, error) {
	sp, rel, err := fs.resolve(ref)
	if err != nil {
		return nil, err
	}
	return fs.info(ctx, sp, rel, true)
}

func (fs *externalFS) ListFolder(ctx context.Context, ref *provider.Reference, _, _ []string) ([]*provider.ResourceInfo, error) {
	sp, rel, err := fs.resolve(ref)
	if err != nil {
		return nil, err
	}
	dir, err := sp.safePath(rel)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errtypes.NotFound("externalfs: not found")
		}
		return nil, err
	}
	var result []*provider.ResourceInfo
	for _, entry := range entries {
		childRel := path.Join(rel, entry.Name())
		ri, err := fs.info(ctx, sp, childRel, false)
		if err == nil {
			result = append(result, ri)
		}
	}
	return result, nil
}

func (fs *externalFS) Download(ctx context.Context, ref *provider.Reference, openReader func(*provider.ResourceInfo) bool) (*provider.ResourceInfo, io.ReadCloser, error) {
	sp, rel, err := fs.resolve(ref)
	if err != nil {
		return nil, nil, err
	}
	ri, err := fs.info(ctx, sp, rel, true)
	if err != nil {
		return nil, nil, err
	}
	if ri.GetType() != provider.ResourceType_RESOURCE_TYPE_FILE {
		return nil, nil, errtypes.BadRequest("externalfs: cannot download a folder")
	}
	if !openReader(ri) {
		return ri, nil, nil
	}
	p, err := sp.safePath(rel)
	if err != nil {
		return nil, nil, err
	}
	file, err := os.Open(p)
	if err != nil {
		return nil, nil, err
	}
	return ri, file, nil
}

func (fs *externalFS) resolve(ref *provider.Reference) (*space, string, error) {
	if ref == nil || ref.ResourceId == nil {
		return nil, "", errtypes.BadRequest("externalfs: resource id is required")
	}
	for _, sp := range fs.spaces {
		if ref.ResourceId.GetSpaceId() != sp.id {
			continue
		}
		rel := "."
		if oid := ref.ResourceId.GetOpaqueId(); oid != "" && oid != sp.id {
			p, err := fs.GetPathByID(context.Background(), ref.ResourceId)
			if err != nil {
				return nil, "", err
			}
			rel = strings.TrimPrefix(p, "/")
		}
		if ref.GetPath() != "" {
			rel = path.Join(rel, ref.GetPath())
		}
		return sp, rel, nil
	}
	return nil, "", errtypes.NotFound("externalfs: space not found")
}

func (sp *space) safePath(rel string) (string, error) {
	cleanRel := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(rel, "/")))
	if cleanRel == "." {
		cleanRel = ""
	}
	candidate, err := filepath.Abs(filepath.Join(sp.root, cleanRel))
	if err != nil {
		return "", err
	}
	if !sameOrChild(candidate, sp.root) {
		return "", errtypes.PermissionDenied("externalfs: path leaves datasource root")
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			return candidate, nil
		}
		return "", err
	}
	if !sameOrChild(resolved, sp.root) {
		return "", errtypes.PermissionDenied("externalfs: symlink leaves datasource root")
	}
	return resolved, nil
}

func (sp *space) safeTargetPath(rel string) (string, error) {
	cleanRel := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(rel, "/")))
	if cleanRel == "." || cleanRel == "" {
		return "", errtypes.PermissionDenied("externalfs: refusing to write datasource root")
	}
	parentRel := filepath.Dir(cleanRel)
	if parentRel == "." {
		parentRel = ""
	}
	parent, err := sp.safePath(parentRel)
	if err != nil {
		return "", err
	}
	parentInfo, err := os.Stat(parent)
	if err != nil {
		if os.IsNotExist(err) {
			return "", errtypes.PreconditionFailed("externalfs: parent folder does not exist")
		}
		return "", err
	}
	if !parentInfo.IsDir() {
		return "", errtypes.PreconditionFailed("externalfs: parent is not a folder")
	}
	target := filepath.Join(parent, filepath.Base(cleanRel))
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		if os.IsNotExist(err) {
			return target, nil
		}
		return "", err
	}
	if !sameOrChild(resolved, sp.root) {
		return "", errtypes.PermissionDenied("externalfs: symlink leaves datasource root")
	}
	return resolved, nil
}

func (sp *space) safeLinkPath(rel string) (string, error) {
	cleanRel := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(rel, "/")))
	if cleanRel == "." || cleanRel == "" {
		return "", errtypes.PermissionDenied("externalfs: refusing to modify datasource root")
	}
	parentRel := filepath.Dir(cleanRel)
	if parentRel == "." {
		parentRel = ""
	}
	parent, err := sp.safePath(parentRel)
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(parent, filepath.Base(cleanRel))
	info, err := os.Lstat(candidate)
	if err != nil {
		return "", mapFileError(err, candidate)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return "", err
		}
		if !sameOrChild(resolved, sp.root) {
			return "", errtypes.PermissionDenied("externalfs: symlink leaves datasource root")
		}
	}
	return candidate, nil
}

func (sp *space) ensureWritable() error {
	if sp.source.ReadOnly {
		return errtypes.PermissionDenied("externalfs: datasource is configured read-only")
	}
	return nil
}

func (sp *space) ensureDestructiveAllowed() error {
	if err := sp.ensureWritable(); err != nil {
		return err
	}
	if !sp.source.AllowDeletes {
		return errtypes.PermissionDenied("externalfs: destructive operations require allow_deletes")
	}
	return nil
}

func mapFileError(err error, p string) error {
	switch {
	case err == nil:
		return nil
	case os.IsNotExist(err):
		return errtypes.NotFound(p)
	case os.IsPermission(err):
		return errtypes.PermissionDenied(p)
	case os.IsExist(err):
		return errtypes.AlreadyExists(p)
	default:
		return err
	}
}

func (fs *externalFS) info(ctx context.Context, sp *space, rel string, includeSpace bool) (*provider.ResourceInfo, error) {
	p, err := sp.safePath(rel)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errtypes.NotFound("externalfs: not found")
		}
		return nil, err
	}
	cleanRel := path.Clean(strings.ReplaceAll(rel, string(filepath.Separator), "/"))
	if cleanRel == "." || cleanRel == "/" {
		cleanRel = ""
	}
	opaqueID := "p:" + url.QueryEscape(cleanRel)
	if cleanRel == "" {
		opaqueID = sp.id
	}
	id := &provider.ResourceId{StorageId: fs.mountID, SpaceId: sp.id, OpaqueId: opaqueID}
	ri := &provider.ResourceInfo{
		Type:          resourceType(fi),
		Id:            id,
		Path:          path.Join("/", cleanRel),
		Name:          path.Base(cleanRel),
		Size:          uint64(fi.Size()),
		Etag:          etag(fi),
		MimeType:      mime.Detect(fi.IsDir(), cleanRel),
		Mtime:         &types.Timestamp{Seconds: uint64(fi.ModTime().Unix()), Nanos: uint32(fi.ModTime().Nanosecond())},
		Owner:         sp.owner(ctx),
		PermissionSet: sp.permissions(),
	}
	if cleanRel == "" {
		ri.Name = sp.name
	}
	parent := path.Dir(cleanRel)
	if parent != "." && parent != "/" && cleanRel != "" {
		ri.ParentId = &provider.ResourceId{StorageId: fs.mountID, SpaceId: sp.id, OpaqueId: "p:" + url.QueryEscape(parent)}
	} else if cleanRel != "" {
		ri.ParentId = &provider.ResourceId{StorageId: fs.mountID, SpaceId: sp.id, OpaqueId: sp.id}
	}
	if includeSpace {
		ri.Space = fs.storageSpace(ctx, sp, false)
	}
	return ri, nil
}

func resourceType(fi os.FileInfo) provider.ResourceType {
	if fi.IsDir() {
		return provider.ResourceType_RESOURCE_TYPE_CONTAINER
	}
	return provider.ResourceType_RESOURCE_TYPE_FILE
}

func (sp *space) permissions() *provider.ResourcePermissions {
	perms := &provider.ResourcePermissions{
		GetPath:              true,
		GetQuota:             true,
		InitiateFileDownload: true,
		ListContainer:        true,
		Stat:                 true,
	}
	if !sp.source.ReadOnly {
		perms.CreateContainer = true
		perms.InitiateFileUpload = true
	}
	if !sp.source.ReadOnly && sp.source.AllowDeletes {
		perms.Delete = true
		perms.Move = true
	}
	return perms
}

func etag(fi os.FileInfo) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%d", fi.ModTime().UnixNano(), fi.Size(), fi.Mode())))
	return `"` + hex.EncodeToString(h[:16]) + `"`
}

func parseMTime(v string) (time.Time, error) {
	p := strings.SplitN(v, ".", 2)
	sec, err := strconv.ParseInt(p[0], 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	var nsec int64
	if len(p) > 1 {
		nsec, err = strconv.ParseInt(p[1], 10, 64)
		if err != nil {
			return time.Time{}, err
		}
	}
	return time.Unix(sec, nsec), nil
}

func unsupported() error {
	return errtypes.NotSupported("externalfs: operation is not supported for external datasources")
}

func (fs *externalFS) CreateReference(context.Context, string, *url.URL) error { return unsupported() }

func (fs *externalFS) CreateDir(_ context.Context, ref *provider.Reference) error {
	sp, rel, err := fs.resolve(ref)
	if err != nil {
		return err
	}
	if err := sp.ensureWritable(); err != nil {
		return err
	}
	p, err := sp.safeTargetPath(rel)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); err == nil {
		return errtypes.AlreadyExists(p)
	} else if !os.IsNotExist(err) {
		return mapFileError(err, p)
	}
	return mapFileError(os.Mkdir(p, 0o755), p)
}

func (fs *externalFS) TouchFile(_ context.Context, ref *provider.Reference, _ bool, mtime string) error {
	sp, rel, err := fs.resolve(ref)
	if err != nil {
		return err
	}
	if err := sp.ensureWritable(); err != nil {
		return err
	}
	p, err := sp.safeTargetPath(rel)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return mapFileError(err, p)
	}
	if err := file.Close(); err != nil {
		return err
	}
	if mtime == "" {
		now := time.Now()
		return mapFileError(os.Chtimes(p, now, now), p)
	}
	t, err := parseMTime(mtime)
	if err != nil {
		return errtypes.BadRequest("externalfs: invalid mtime")
	}
	return mapFileError(os.Chtimes(p, t, t), p)
}

func (fs *externalFS) Delete(_ context.Context, ref *provider.Reference) error {
	sp, rel, err := fs.resolve(ref)
	if err != nil {
		return err
	}
	if err := sp.ensureDestructiveAllowed(); err != nil {
		return err
	}
	if filepath.Clean(filepath.FromSlash(strings.TrimPrefix(rel, "/"))) == "." {
		return errtypes.PermissionDenied("externalfs: refusing to delete datasource root")
	}
	p, err := sp.safeLinkPath(rel)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); err != nil {
		return mapFileError(err, p)
	}
	return mapFileError(os.RemoveAll(p), p)
}

func (fs *externalFS) Move(ctx context.Context, oldRef, newRef *provider.Reference) error {
	oldSpace, oldRel, err := fs.resolve(oldRef)
	if err != nil {
		return err
	}
	newSpace, newRel, err := fs.resolve(newRef)
	if err != nil {
		return err
	}
	if oldSpace.id != newSpace.id {
		return errtypes.PermissionDenied("externalfs: moving between external spaces is not supported")
	}
	if err := oldSpace.ensureDestructiveAllowed(); err != nil {
		return err
	}
	if filepath.Clean(filepath.FromSlash(strings.TrimPrefix(oldRel, "/"))) == "." {
		return errtypes.PermissionDenied("externalfs: refusing to move datasource root")
	}
	oldPath, err := oldSpace.safeLinkPath(oldRel)
	if err != nil {
		return err
	}
	newPath, err := newSpace.safeTargetPath(newRel)
	if err != nil {
		return err
	}
	if _, err := os.Stat(newPath); err == nil {
		return errtypes.AlreadyExists(newPath)
	} else if !os.IsNotExist(err) {
		return mapFileError(err, newPath)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		return mapFileError(err, newPath)
	}
	_, err = fs.info(ctx, newSpace, newRel, false)
	return err
}

func (fs *externalFS) InitiateUpload(ctx context.Context, ref *provider.Reference, uploadLength int64, _ map[string]string) (map[string]string, error) {
	sp, _, err := fs.resolve(ref)
	if err != nil {
		return nil, err
	}
	if err := sp.ensureWritable(); err != nil {
		return nil, err
	}
	if uploadLength == 0 {
		if _, err := fs.upload(ctx, storage.UploadRequest{Ref: ref, Body: io.NopCloser(strings.NewReader(""))}, nil); err != nil {
			return nil, err
		}
	}
	uploadID, err := storagespace.FormatReference(ref)
	if err != nil {
		return nil, err
	}
	spaceUploadID := path.Join(storagespace.FormatResourceID(ref.GetResourceId()), ref.GetPath())
	return map[string]string{
		"simple": uploadID,
		"spaces": spaceUploadID,
	}, nil
}

func (fs *externalFS) Upload(ctx context.Context, req storage.UploadRequest, uploadFunc storage.UploadFinishedFunc) (*provider.ResourceInfo, error) {
	return fs.upload(ctx, req, uploadFunc)
}

func (fs *externalFS) upload(ctx context.Context, req storage.UploadRequest, uploadFunc storage.UploadFinishedFunc) (*provider.ResourceInfo, error) {
	sp, rel, err := fs.resolveUploadRef(req.Ref)
	if err != nil {
		return nil, err
	}
	if err := sp.ensureWritable(); err != nil {
		return nil, err
	}
	p, err := sp.safeTargetPath(rel)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, mapFileError(err, p)
	}
	_, copyErr := io.Copy(file, req.Body)
	closeErr := file.Close()
	if copyErr != nil {
		return nil, copyErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	ri, err := fs.info(ctx, sp, rel, true)
	if err != nil {
		return nil, err
	}
	if uploadFunc != nil {
		executant := sp.owner(ctx)
		if u, ok := ctxpkg.ContextGetUser(ctx); ok && u.GetId() != nil {
			executant = u.GetId()
		}
		uploadFunc(sp.owner(ctx), executant, &provider.Reference{ResourceId: ri.GetId()})
	}
	return ri, nil
}

func (fs *externalFS) resolveUploadRef(ref *provider.Reference) (*space, string, error) {
	if ref != nil && ref.ResourceId != nil {
		return fs.resolve(ref)
	}
	if ref == nil || ref.GetPath() == "" {
		return nil, "", errtypes.BadRequest("externalfs: upload reference is required")
	}
	parsed, err := storagespace.ParseReference(strings.TrimLeft(ref.GetPath(), "/"))
	if err != nil {
		return nil, "", err
	}
	return fs.resolve(&parsed)
}
func (fs *externalFS) ListRevisions(context.Context, *provider.Reference) ([]*provider.FileVersion, error) {
	return nil, unsupported()
}
func (fs *externalFS) DownloadRevision(context.Context, *provider.Reference, string, func(*provider.ResourceInfo) bool) (*provider.ResourceInfo, io.ReadCloser, error) {
	return nil, nil, unsupported()
}
func (fs *externalFS) RestoreRevision(context.Context, *provider.Reference, string) error { return unsupported() }
func (fs *externalFS) ListRecycle(context.Context, *provider.Reference, string, string) ([]*provider.RecycleItem, error) {
	return nil, unsupported()
}
func (fs *externalFS) RestoreRecycleItem(context.Context, *provider.Reference, string, string, *provider.Reference) error {
	return unsupported()
}
func (fs *externalFS) PurgeRecycleItem(context.Context, *provider.Reference, string, string) error { return unsupported() }
func (fs *externalFS) EmptyRecycle(context.Context, *provider.Reference) error { return unsupported() }
func (fs *externalFS) AddGrant(context.Context, *provider.Reference, *provider.Grant) error { return unsupported() }
func (fs *externalFS) DenyGrant(context.Context, *provider.Reference, *provider.Grantee) error { return unsupported() }
func (fs *externalFS) RemoveGrant(context.Context, *provider.Reference, *provider.Grant) error { return unsupported() }
func (fs *externalFS) UpdateGrant(context.Context, *provider.Reference, *provider.Grant) error { return unsupported() }
func (fs *externalFS) ListGrants(context.Context, *provider.Reference) ([]*provider.Grant, error) {
	return nil, unsupported()
}
func (fs *externalFS) SetArbitraryMetadata(_ context.Context, ref *provider.Reference, md *provider.ArbitraryMetadata) error {
	if md == nil || len(md.Metadata) == 0 {
		return nil
	}
	mtime, ok := md.Metadata["mtime"]
	if !ok || len(md.Metadata) != 1 {
		return unsupported()
	}
	sp, rel, err := fs.resolve(ref)
	if err != nil {
		return err
	}
	if err := sp.ensureWritable(); err != nil {
		return err
	}
	t, err := parseMTime(mtime)
	if err != nil {
		return errtypes.BadRequest("externalfs: invalid mtime")
	}
	p, err := sp.safePath(rel)
	if err != nil {
		return err
	}
	return mapFileError(os.Chtimes(p, t, t), p)
}
func (fs *externalFS) UnsetArbitraryMetadata(context.Context, *provider.Reference, []string) error {
	return unsupported()
}
func (fs *externalFS) AddLabel(context.Context, *provider.Reference, *userv1beta1.UserId, string) error {
	return unsupported()
}
func (fs *externalFS) RemoveLabel(context.Context, *provider.Reference, *userv1beta1.UserId, string) error {
	return unsupported()
}
func (fs *externalFS) GetLock(context.Context, *provider.Reference) (*provider.Lock, error) {
	return nil, unsupported()
}
func (fs *externalFS) SetLock(context.Context, *provider.Reference, *provider.Lock) error { return unsupported() }
func (fs *externalFS) RefreshLock(context.Context, *provider.Reference, *provider.Lock, string) error {
	return unsupported()
}
func (fs *externalFS) Unlock(context.Context, *provider.Reference, *provider.Lock) error { return unsupported() }

func (fs *externalFS) CreateStorageSpace(ctx context.Context, _ *provider.CreateStorageSpaceRequest) (*provider.CreateStorageSpaceResponse, error) {
	return &provider.CreateStorageSpaceResponse{Status: status.NewUnimplemented(ctx, nil, "external datasource spaces are configured, not user-created")}, nil
}
func (fs *externalFS) UpdateStorageSpace(ctx context.Context, _ *provider.UpdateStorageSpaceRequest) (*provider.UpdateStorageSpaceResponse, error) {
	return &provider.UpdateStorageSpaceResponse{Status: status.NewUnimplemented(ctx, nil, "external datasource space metadata is not managed here")}, nil
}
func (fs *externalFS) DeleteStorageSpace(context.Context, *provider.DeleteStorageSpaceRequest) error {
	return errtypes.PermissionDenied("externalfs: remove external spaces from the datasource configuration")
}
func (fs *externalFS) CreateHome(context.Context) error { return unsupported() }
func (fs *externalFS) GetHome(context.Context) (string, error) { return "", unsupported() }

func (fs *externalFS) hasSpace(spaceID string) bool {
	for _, sp := range fs.spaces {
		if sp.id == spaceID {
			return true
		}
	}
	return false
}

func (fs *compositeFS) Shutdown(ctx context.Context) error {
	if err := fs.primary.Shutdown(ctx); err != nil {
		return err
	}
	return fs.external.Shutdown(ctx)
}

func (fs *compositeFS) ListStorageSpaces(ctx context.Context, filters []*provider.ListStorageSpacesRequest_Filter, unrestricted bool) ([]*provider.StorageSpace, error) {
	if id := requestedSpaceID(filters); id != "" && fs.external.hasSpace(id) {
		return fs.external.ListStorageSpaces(ctx, filters, unrestricted)
	}
	primary, err := fs.primary.ListStorageSpaces(ctx, filters, unrestricted)
	if err != nil {
		return nil, err
	}
	external, err := fs.external.ListStorageSpaces(ctx, filters, unrestricted)
	if err != nil {
		return nil, err
	}
	return append(primary, external...), nil
}

func requestedSpaceID(filters []*provider.ListStorageSpacesRequest_Filter) string {
	for _, f := range filters {
		if f.Type == provider.ListStorageSpacesRequest_Filter_TYPE_ID {
			_, id := splitStorageID(f.GetId().GetOpaqueId())
			return id
		}
	}
	return ""
}

func (fs *compositeFS) isExternalRef(ref *provider.Reference) bool {
	return ref != nil && ref.ResourceId != nil && fs.external.hasSpace(ref.ResourceId.GetSpaceId())
}

func (fs *compositeFS) isExternalResourceID(id *provider.ResourceId) bool {
	return id != nil && fs.external.hasSpace(id.GetSpaceId())
}

func (fs *compositeFS) GetQuota(ctx context.Context, ref *provider.Reference) (uint64, uint64, uint64, error) {
	if fs.isExternalRef(ref) {
		return fs.external.GetQuota(ctx, ref)
	}
	return fs.primary.GetQuota(ctx, ref)
}

func (fs *compositeFS) GetMD(ctx context.Context, ref *provider.Reference, mdKeys, fieldMask []string) (*provider.ResourceInfo, error) {
	if fs.isExternalRef(ref) {
		return fs.external.GetMD(ctx, ref, mdKeys, fieldMask)
	}
	return fs.primary.GetMD(ctx, ref, mdKeys, fieldMask)
}

func (fs *compositeFS) ListFolder(ctx context.Context, ref *provider.Reference, mdKeys, fieldMask []string) ([]*provider.ResourceInfo, error) {
	if fs.isExternalRef(ref) {
		return fs.external.ListFolder(ctx, ref, mdKeys, fieldMask)
	}
	return fs.primary.ListFolder(ctx, ref, mdKeys, fieldMask)
}

func (fs *compositeFS) Download(ctx context.Context, ref *provider.Reference, openReader func(*provider.ResourceInfo) bool) (*provider.ResourceInfo, io.ReadCloser, error) {
	if fs.isExternalRef(ref) {
		return fs.external.Download(ctx, ref, openReader)
	}
	return fs.primary.Download(ctx, ref, openReader)
}

func (fs *compositeFS) GetPathByID(ctx context.Context, id *provider.ResourceId) (string, error) {
	if fs.isExternalResourceID(id) {
		return fs.external.GetPathByID(ctx, id)
	}
	return fs.primary.GetPathByID(ctx, id)
}

func (fs *compositeFS) CreateReference(ctx context.Context, p string, targetURI *url.URL) error {
	return fs.primary.CreateReference(ctx, p, targetURI)
}

func (fs *compositeFS) CreateDir(ctx context.Context, ref *provider.Reference) error {
	if fs.isExternalRef(ref) {
		return fs.external.CreateDir(ctx, ref)
	}
	return fs.primary.CreateDir(ctx, ref)
}

func (fs *compositeFS) TouchFile(ctx context.Context, ref *provider.Reference, markprocessing bool, mtime string) error {
	if fs.isExternalRef(ref) {
		return fs.external.TouchFile(ctx, ref, markprocessing, mtime)
	}
	return fs.primary.TouchFile(ctx, ref, markprocessing, mtime)
}

func (fs *compositeFS) Delete(ctx context.Context, ref *provider.Reference) error {
	if fs.isExternalRef(ref) {
		return fs.external.Delete(ctx, ref)
	}
	return fs.primary.Delete(ctx, ref)
}

func (fs *compositeFS) Move(ctx context.Context, oldRef, newRef *provider.Reference) error {
	oldExternal := fs.isExternalRef(oldRef)
	newExternal := fs.isExternalRef(newRef)
	if oldExternal && newExternal {
		return fs.external.Move(ctx, oldRef, newRef)
	}
	if oldExternal || newExternal {
		return unsupported()
	}
	return fs.primary.Move(ctx, oldRef, newRef)
}

func (fs *compositeFS) InitiateUpload(ctx context.Context, ref *provider.Reference, uploadLength int64, metadata map[string]string) (map[string]string, error) {
	if fs.isExternalRef(ref) {
		return fs.external.InitiateUpload(ctx, ref, uploadLength, metadata)
	}
	return fs.primary.InitiateUpload(ctx, ref, uploadLength, metadata)
}

func (fs *compositeFS) Upload(ctx context.Context, req storage.UploadRequest, uploadFunc storage.UploadFinishedFunc) (*provider.ResourceInfo, error) {
	if fs.isExternalRef(req.Ref) {
		return fs.external.Upload(ctx, req, uploadFunc)
	}
	return fs.primary.Upload(ctx, req, uploadFunc)
}

func (fs *compositeFS) ListRevisions(ctx context.Context, ref *provider.Reference) ([]*provider.FileVersion, error) {
	if fs.isExternalRef(ref) {
		return fs.external.ListRevisions(ctx, ref)
	}
	return fs.primary.ListRevisions(ctx, ref)
}

func (fs *compositeFS) DownloadRevision(ctx context.Context, ref *provider.Reference, key string, openReader func(*provider.ResourceInfo) bool) (*provider.ResourceInfo, io.ReadCloser, error) {
	if fs.isExternalRef(ref) {
		return fs.external.DownloadRevision(ctx, ref, key, openReader)
	}
	return fs.primary.DownloadRevision(ctx, ref, key, openReader)
}

func (fs *compositeFS) RestoreRevision(ctx context.Context, ref *provider.Reference, key string) error {
	if fs.isExternalRef(ref) {
		return fs.external.RestoreRevision(ctx, ref, key)
	}
	return fs.primary.RestoreRevision(ctx, ref, key)
}

func (fs *compositeFS) ListRecycle(ctx context.Context, ref *provider.Reference, key, relativePath string) ([]*provider.RecycleItem, error) {
	if fs.isExternalRef(ref) {
		return fs.external.ListRecycle(ctx, ref, key, relativePath)
	}
	return fs.primary.ListRecycle(ctx, ref, key, relativePath)
}

func (fs *compositeFS) RestoreRecycleItem(ctx context.Context, ref *provider.Reference, key, relativePath string, restoreRef *provider.Reference) error {
	if fs.isExternalRef(ref) || fs.isExternalRef(restoreRef) {
		return unsupported()
	}
	return fs.primary.RestoreRecycleItem(ctx, ref, key, relativePath, restoreRef)
}

func (fs *compositeFS) PurgeRecycleItem(ctx context.Context, ref *provider.Reference, key, relativePath string) error {
	if fs.isExternalRef(ref) {
		return fs.external.PurgeRecycleItem(ctx, ref, key, relativePath)
	}
	return fs.primary.PurgeRecycleItem(ctx, ref, key, relativePath)
}

func (fs *compositeFS) EmptyRecycle(ctx context.Context, ref *provider.Reference) error {
	if fs.isExternalRef(ref) {
		return fs.external.EmptyRecycle(ctx, ref)
	}
	return fs.primary.EmptyRecycle(ctx, ref)
}

func (fs *compositeFS) AddGrant(ctx context.Context, ref *provider.Reference, g *provider.Grant) error {
	if fs.isExternalRef(ref) {
		return fs.external.AddGrant(ctx, ref, g)
	}
	return fs.primary.AddGrant(ctx, ref, g)
}

func (fs *compositeFS) DenyGrant(ctx context.Context, ref *provider.Reference, g *provider.Grantee) error {
	if fs.isExternalRef(ref) {
		return fs.external.DenyGrant(ctx, ref, g)
	}
	return fs.primary.DenyGrant(ctx, ref, g)
}

func (fs *compositeFS) RemoveGrant(ctx context.Context, ref *provider.Reference, g *provider.Grant) error {
	if fs.isExternalRef(ref) {
		return fs.external.RemoveGrant(ctx, ref, g)
	}
	return fs.primary.RemoveGrant(ctx, ref, g)
}

func (fs *compositeFS) UpdateGrant(ctx context.Context, ref *provider.Reference, g *provider.Grant) error {
	if fs.isExternalRef(ref) {
		return fs.external.UpdateGrant(ctx, ref, g)
	}
	return fs.primary.UpdateGrant(ctx, ref, g)
}

func (fs *compositeFS) ListGrants(ctx context.Context, ref *provider.Reference) ([]*provider.Grant, error) {
	if fs.isExternalRef(ref) {
		return fs.external.ListGrants(ctx, ref)
	}
	return fs.primary.ListGrants(ctx, ref)
}

func (fs *compositeFS) SetArbitraryMetadata(ctx context.Context, ref *provider.Reference, md *provider.ArbitraryMetadata) error {
	if fs.isExternalRef(ref) {
		return fs.external.SetArbitraryMetadata(ctx, ref, md)
	}
	return fs.primary.SetArbitraryMetadata(ctx, ref, md)
}

func (fs *compositeFS) UnsetArbitraryMetadata(ctx context.Context, ref *provider.Reference, keys []string) error {
	if fs.isExternalRef(ref) {
		return fs.external.UnsetArbitraryMetadata(ctx, ref, keys)
	}
	return fs.primary.UnsetArbitraryMetadata(ctx, ref, keys)
}

func (fs *compositeFS) AddLabel(ctx context.Context, ref *provider.Reference, userID *userv1beta1.UserId, label string) error {
	if fs.isExternalRef(ref) {
		return fs.external.AddLabel(ctx, ref, userID, label)
	}
	return fs.primary.AddLabel(ctx, ref, userID, label)
}

func (fs *compositeFS) RemoveLabel(ctx context.Context, ref *provider.Reference, userID *userv1beta1.UserId, label string) error {
	if fs.isExternalRef(ref) {
		return fs.external.RemoveLabel(ctx, ref, userID, label)
	}
	return fs.primary.RemoveLabel(ctx, ref, userID, label)
}

func (fs *compositeFS) GetLock(ctx context.Context, ref *provider.Reference) (*provider.Lock, error) {
	if fs.isExternalRef(ref) {
		return fs.external.GetLock(ctx, ref)
	}
	return fs.primary.GetLock(ctx, ref)
}

func (fs *compositeFS) SetLock(ctx context.Context, ref *provider.Reference, lock *provider.Lock) error {
	if fs.isExternalRef(ref) {
		return fs.external.SetLock(ctx, ref, lock)
	}
	return fs.primary.SetLock(ctx, ref, lock)
}

func (fs *compositeFS) RefreshLock(ctx context.Context, ref *provider.Reference, lock *provider.Lock, existingLockID string) error {
	if fs.isExternalRef(ref) {
		return fs.external.RefreshLock(ctx, ref, lock, existingLockID)
	}
	return fs.primary.RefreshLock(ctx, ref, lock, existingLockID)
}

func (fs *compositeFS) Unlock(ctx context.Context, ref *provider.Reference, lock *provider.Lock) error {
	if fs.isExternalRef(ref) {
		return fs.external.Unlock(ctx, ref, lock)
	}
	return fs.primary.Unlock(ctx, ref, lock)
}

func (fs *compositeFS) CreateStorageSpace(ctx context.Context, req *provider.CreateStorageSpaceRequest) (*provider.CreateStorageSpaceResponse, error) {
	return fs.primary.CreateStorageSpace(ctx, req)
}

func (fs *compositeFS) UpdateStorageSpace(ctx context.Context, req *provider.UpdateStorageSpaceRequest) (*provider.UpdateStorageSpaceResponse, error) {
	if req != nil && req.GetStorageSpace() != nil {
		if root := req.GetStorageSpace().GetRoot(); fs.isExternalResourceID(root) {
			return fs.external.UpdateStorageSpace(ctx, req)
		}
		_, id := splitStorageID(req.GetStorageSpace().GetId().GetOpaqueId())
		if fs.external.hasSpace(id) {
			return fs.external.UpdateStorageSpace(ctx, req)
		}
	}
	return fs.primary.UpdateStorageSpace(ctx, req)
}

func (fs *compositeFS) DeleteStorageSpace(ctx context.Context, req *provider.DeleteStorageSpaceRequest) error {
	if req != nil {
		_, id := splitStorageID(req.GetId().GetOpaqueId())
		if fs.external.hasSpace(id) {
			return fs.external.DeleteStorageSpace(ctx, req)
		}
	}
	return fs.primary.DeleteStorageSpace(ctx, req)
}

func (fs *compositeFS) CreateHome(ctx context.Context) error {
	return fs.primary.CreateHome(ctx)
}

func (fs *compositeFS) GetHome(ctx context.Context) (string, error) {
	return fs.primary.GetHome(ctx)
}

func (fs *compositeFS) ListUploadSessions(ctx context.Context, filter storage.UploadSessionFilter) ([]storage.UploadSession, error) {
	lister, ok := fs.primary.(storage.UploadSessionLister)
	if !ok {
		return nil, unsupported()
	}
	return lister.ListUploadSessions(ctx, filter)
}

func (fs *compositeFS) UseIn(composer *tusd.StoreComposer) {
	if composable, ok := fs.primary.(storage.ComposableFS); ok {
		composable.UseIn(composer)
	}
}

func (fs *compositeFS) NewUpload(ctx context.Context, info tusd.FileInfo) (tusd.Upload, error) {
	datastore, ok := fs.primary.(tusd.DataStore)
	if !ok {
		return nil, unsupported()
	}
	return datastore.NewUpload(ctx, info)
}

func (fs *compositeFS) GetUpload(ctx context.Context, id string) (tusd.Upload, error) {
	datastore, ok := fs.primary.(tusd.DataStore)
	if !ok {
		return nil, unsupported()
	}
	return datastore.GetUpload(ctx, id)
}

var _ storage.FS = (*externalFS)(nil)
var _ storage.FS = (*compositeFS)(nil)
var _ storage.UploadSessionLister = (*compositeFS)(nil)
var _ storage.ComposableFS = (*compositeFS)(nil)
var _ tusd.DataStore = (*compositeFS)(nil)
