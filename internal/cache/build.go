package cache

import (
	"path/filepath"
)

// FrontendBuildDir is build/frontend-<sha12>-<cfghash>, the transient copy
// of the frontend tree at commit sha that the build of the image for config
// hash cfghash writes its .env into (§7.2 step 4). It isn't created: the
// build makes and removes it while holding that image's lock.
func (c *Cache) FrontendBuildDir(sha, cfghash string) (string, error) {
	if err := checkSHA(sha); err != nil {
		return "", err
	}
	if err := checkName("config hash", cfghash); err != nil {
		return "", err
	}
	return filepath.Join(c.root, buildDir, "frontend-"+sha[:12]+"-"+cfghash), nil
}
