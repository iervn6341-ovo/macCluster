//go:build !darwin

package runtime

import (
	"os"
	"time"
)

// birthTime is unknown here, so a followed file is always read from where it ends now.
func birthTime(os.FileInfo) time.Time { return time.Time{} }
