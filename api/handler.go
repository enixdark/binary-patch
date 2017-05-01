package api

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	"github.com/szuecs/binary-patch/internal/binarydist" // copied from github.com/inconshreveable/go-update/internal/binarydist
)

var (
	basedir                = "/tmp/bindata" // TODO: choose by cli flag
	errVersionMissing      = errors.New("Missing 'version' in query string")
	errArchitectureMissing = errors.New("Missing 'arch' in query string")
	errOSMissing           = errors.New("Missing 'os' in query string")
	errUnsupportedArchOS   = errors.New("Wrong architecture and OS combination")
	errBinaryNotFound      = errors.New("Binary not found: ")
	supported              = map[ArchAndOS]bool{
		ArchAndOS{Arch: "amd64", OS: "linux"}: true,
	}
)

type Update struct {
	Name    string
	Version string
	System  ArchAndOS
}

type ArchAndOS struct {
	Arch string // GOARCH
	OS   string // GOOS
}

func newUpdate(ginCtx *gin.Context) (*Update, error) {
	version, ok := ginCtx.GetQuery("version")
	if !ok {
		return nil, errVersionMissing
	}
	goarch, ok := ginCtx.GetQuery("arch")
	if !ok {
		return nil, errArchitectureMissing
	}
	goos, ok := ginCtx.GetQuery("os")
	if !ok {
		return nil, errOSMissing
	}
	return &Update{
		Name:    ginCtx.Param("name"),
		Version: version,
		System: ArchAndOS{
			Arch: goarch,
			OS:   goos,
		},
	}, nil
}

func (u *Update) Clone() *Update {
	return &Update{
		Name:    u.Name,
		Version: u.Version,
		System: ArchAndOS{
			Arch: u.System.Arch,
			OS:   u.System.OS,
		},
	}
}

func (u *Update) isSupported() bool {
	_, ok := supported[u.System]
	return ok
}

func (u *Update) getFilepath() string {
	return basedir + "/" + u.Name + "_" + u.Version + "_" + u.System.Arch + u.System.OS
}

func (u *Update) GetReader() (io.ReadCloser, error) {
	filepath := u.getFilepath()
	fd, err := os.Open(filepath)
	if err != nil {
		return nil, errors.Wrap(errBinaryNotFound, filepath)
	}
	return fd, nil
}

func (u *Update) String() string {
	return u.Name + "_" + u.Version + "_" + u.System.Arch + u.System.OS
}

func (u *Update) versionFromFilename(filepath string) string {
	return filepath[len(basedir)+1+len(u.Name)+1 : strings.LastIndex(filepath, "_")]
}

func (u *Update) GetLatestVersion() string {
	globStr := strings.Replace(u.String(), u.Version, "*", 1)
	glog.V(2).Infof("Use globStr: %s", globStr)
	files, err := filepath.Glob(fmt.Sprintf("%s/%s", basedir, globStr))
	if err != nil {
		glog.Errorf("Could not glob filepath '%s', caused by: %v", globStr, err)
		return ""
	}
	glog.V(2).Infof("found %d files by glob", len(files))
	latest := u.Version
	for _, fname := range files {
		v := u.versionFromFilename(fname)
		if v > latest {
			latest = v
		}
	}
	return latest
}

func newUpdateFromCtx(ginCtx *gin.Context) *Update {
	update, err := newUpdate(ginCtx)
	if err != nil {
		ginCtx.AbortWithError(http.StatusBadRequest, err)
	}

	if !update.isSupported() {
		ginCtx.AbortWithError(http.StatusBadRequest, errUnsupportedArchOS)
	}
	return update
}

// UpdateHandler handles /update/:name endpoint
func (svc *Service) UpdateHandler(ginCtx *gin.Context) {
	update := newUpdateFromCtx(ginCtx)
	curVersion := update.Version
	if latest := update.GetLatestVersion(); latest != "" {
		update.Version = latest
	}
	glog.V(2).Infof("client hast version %s, we have latest version %s", curVersion, update.Version)
	if update.Version == curVersion {
		ginCtx.String(http.StatusNotModified, "")
		return
	}
	rc, err := update.GetReader()
	if err != nil {
		ginCtx.AbortWithError(http.StatusInternalServerError, err) // TODO: AbortWithError creates StackTraces, we want to have 4xx and an error log
	}
	defer rc.Close()
	n, err := io.Copy(ginCtx.Writer, rc)
	if err != nil {
		ginCtx.AbortWithError(http.StatusInternalServerError, fmt.Errorf("Could not copy %s to client, caused by: %v", update.Name, err))
	}
	glog.Infof("Copied %d bytes to client to update %s", n, update)
}

// PatchUpdateHandler handles /patch-update/:name endpoint
func (svc *Service) PatchUpdateHandler(ginCtx *gin.Context) {
	newUpdate := newUpdateFromCtx(ginCtx)
	latestVersion := newUpdate.GetLatestVersion()
	glog.V(2).Infof("client has version %s, we have latest version %s", newUpdate.Version, latestVersion)
	if newUpdate.Version == latestVersion {
		ginCtx.String(http.StatusNotModified, "")
		return
	}
	oldUpdate := newUpdate.Clone()

	newUpdate.Version = latestVersion
	rcNew, err := newUpdate.GetReader()
	if err != nil {
		ginCtx.AbortWithError(http.StatusInternalServerError, err) // TODO: AbortWithError creates StackTraces, we want to have 4xx and an error log
	}
	defer rcNew.Close()

	glog.Infof("old: %v, new: %v", oldUpdate, newUpdate)
	rcOld, err := oldUpdate.GetReader()
	if err != nil {
		ginCtx.AbortWithError(http.StatusInternalServerError, err) // TODO: AbortWithError creates StackTraces, we want to have 4xx and an error log
	}
	defer rcOld.Close()

	buf := bytes.NewBuffer(nil)
	rw := bufio.NewReadWriter(bufio.NewReader(buf), bufio.NewWriter(buf))

	err = binarydist.Diff(rcOld, rcNew, rw)
	if err != nil {
		ginCtx.AbortWithError(http.StatusInternalServerError, fmt.Errorf("failed to create a binary patch for %s: %v", newUpdate.Name, err))
	}
	rw.Flush()
	n, err := io.Copy(ginCtx.Writer, rw)
	if err != nil {
		ginCtx.AbortWithError(http.StatusInternalServerError, fmt.Errorf("failed to copy %s to client: %v", newUpdate.Name, err))
	}

	glog.Infof("Copied %d bytes to client to patch %s", n, newUpdate)

}

// SignedUpdateHandler handles /signed-update/:name endpoint
func (svc *Service) SignedUpdateHandler(ginCtx *gin.Context) {
	ginCtx.AbortWithError(http.StatusInternalServerError, fmt.Errorf("not implemented"))
}

// RootHandler handles / endpoint
func (svc *Service) RootHandler(ginCtx *gin.Context) {
	ginCtx.JSON(http.StatusOK, gin.H{"title": "root"})
}

// HealthHandler handles /healthz endpoint
func (svc *Service) HealthHandler(ginCtx *gin.Context) {
	if svc.IsHealthy() {
		ginCtx.String(http.StatusOK, "%s", "OK")
	} else {
		ginCtx.String(http.StatusServiceUnavailable, "%s", "Unavailable")
	}
}

// IsHealthy returns the health status of the running service.
func (svc *Service) IsHealthy() bool {
	return svc.Healthy
}
