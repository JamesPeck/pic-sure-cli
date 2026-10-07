package cache

import (
	"path/filepath"
)

// FrontendBuildDir is build/frontend-<tag>, the transient copy of the
// frontend tree that the build of pic-sure-httpd:<tag> writes its .env into
// (§7.2 step 4), e.g. build/frontend-<sha12>-<cfghash8>. It isn't created:
// the build makes and removes it while holding that image's lock.
func (c *Cache) FrontendBuildDir(tag string) (string, error) {
	if err := checkName("image tag", tag); err != nil {
		return "", err
	}
	return filepath.Join(c.root, buildDir, "frontend-"+tag), nil
}
