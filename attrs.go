package acp

import (
	"fmt"
	"os"
)

func CopyAttrs(dst, src string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("get src stat failed, path= %q, %w", src, err)
	}

	stat, err := newStat(src, fi)
	if err != nil {
		return fmt.Errorf("new stat failed, path= %q, %w", src, err)
	}

	if err := writeSysStat(dst, stat); err != nil {
		return fmt.Errorf("write sys stat failed, path= %q, %w", dst, err)
	}

	return nil
}
