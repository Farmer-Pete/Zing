//go:build !darwin && !linux

package proc

// startToken has no implementation outside darwin and linux (design
// section 6.1): every caller goes through StartToken, which this makes
// return ErrUnsupported on any other platform, so the package still builds
// everywhere even though it answers nothing there.
func startToken(_ int) (string, error) {
	return "", ErrUnsupported
}
