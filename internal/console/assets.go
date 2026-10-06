package console

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
)

// assetVersionLen is how many hex characters of the sha256 the ?v= query
// keeps (#59, Q2 decision).
const assetVersionLen = 12

// The two Cache-Control values staticAsset chooses between: immutable for a
// URL whose v is this build's assetVersion, no-cache for every other static
// URL and for GET / (#59).
const (
	cacheControlImmutable = "public, max-age=31536000, immutable"
	cacheControlNoCache   = "no-cache"
)

// hashAssets is the asset version for one set of embedded files: the
// sha256 of their bytes joined in order, hex encoded, cut to
// assetVersionLen. It changes exactly when what the browser runs changes,
// unlike the commit token zing version prints, which stays the same across
// patched builds of one commit (#59, Q2 decision).
func hashAssets(assets ...[]byte) string {
	sum := sha256.Sum256(bytes.Join(assets, nil))
	return hex.EncodeToString(sum[:])[:assetVersionLen]
}

// assetVersion is the ?v= value on every static URL this binary renders,
// and the value every /stream frame and the shell's body carry as
// data-build (#59).
var assetVersion = hashAssets(datastarJS, mermaidJS, consoleJS, keyboardMJS, keysJSON)

// AssetVersion returns the 12-character asset version this binary puts in
// every /static/ URL and in data-build (#59).
func AssetVersion() string { return assetVersion }
