package util

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/golang/glog"
)

// IsDevice returns whether the path is a device
func IsDevice(path string) bool {
	glog.V(3).Infof("IsDevice, path: %s", path)

	fi, err := os.Lstat(path)
	if err != nil {
		glog.Errorf("Bad stat: %v", err)
		return false
	}

	switch mode := fi.Mode(); {
	case mode&os.ModeCharDevice == os.ModeCharDevice:
		glog.V(2).Infof("path: %s, is a char device", path)
		return true
	case mode&os.ModeDevice == os.ModeDevice:
		glog.V(2).Infof("path: %s, is a device", path)
		return true
	default:
		glog.V(2).Infof("path: %s, is not a device", path)
		return false
	}
}

// IsSymlink returns whether the path is a symlink
func IsSymlink(path string) bool {
	glog.V(3).Infof("IsSymlink, path: %s", path)

	fi, err := os.Lstat(path)
	if err != nil {
		glog.Errorf("Bad stat: %v", err)
		return false
	}

	if fi.Mode()&os.ModeSymlink == os.ModeSymlink {
		glog.V(2).Infof("path: %s, is a symlink", path)
		return true
	}

	glog.V(2).Infof("path: %s, is not a symlink", path)

	return false
}

// FollowSymlink returns the path of a symlink, error otherwise
func FollowSymlink(path string) (string, error) {
	glog.V(3).Infof("FollowSymlink, path: %s", path)

	if !IsSymlink(path) {
		return "", fmt.Errorf("not a symlink")
	}

	symPath, err := os.Readlink(path)
	if err != nil {
		return "", err
	}

	glog.V(2).Infof("symPath: %s", symPath)

	if !filepath.IsAbs(symPath) {
		symPath = filepath.Join(filepath.Dir(path), symPath)
		glog.V(2).Infof("Absolute symPath: %s", symPath)
	}

	glog.V(2).Infof("checking that symPath: %s, is valid", symPath)

	_, err = os.Stat(symPath)
	if err != nil {
		return "", err
	}

	glog.V(2).Infof("symPath: %s, exists", symPath)

	return symPath, nil
}
