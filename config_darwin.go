package bearpush

import (
	"filepath"
	"os"
)

var defaultUserConfigRoot = filepath.Join(os.Getenv("HOME"), "Library", "Application Support")
