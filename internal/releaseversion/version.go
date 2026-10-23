// Package releaseversion defines the stable and isolated staging release versions.
package releaseversion

import (
	"errors"
	"strconv"
	"strings"
)

type Version struct {
	core    [3]uint64
	rc      uint64
	staging bool
}

func Parse(value string) (Version, error) {
	var result Version
	if len(value) > 64 {
		return result, errors.New("release version exceeds the registration metadata limit")
	}
	value = strings.TrimPrefix(value, "v")
	core, candidate, staging := strings.Cut(value, "-rc.")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return result, errors.New("expected major.minor.patch or major.minor.patch-rc.N")
	}
	for i, part := range parts {
		n, err := number(part)
		if err != nil {
			return result, err
		}
		result.core[i] = n
	}
	if staging {
		n, err := number(candidate)
		if err != nil {
			return result, err
		}
		result.rc, result.staging = n, true
	}
	return result, nil
}

func number(value string) (uint64, error) {
	if value == "" || len(value) > 1 && value[0] == '0' {
		return 0, errors.New("invalid release version component")
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, errors.New("invalid release version component")
		}
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, errors.New("release version component exceeds the supported range")
	}
	return n, nil
}

func (v Version) Channel() string {
	if v.staging {
		return "staging"
	}
	return "stable"
}

// Floor is a same-channel baseline for offline verification and smoke fixtures.
func (v Version) Floor() string {
	if v.staging {
		return "0.0.0-rc.0"
	}
	return "0.0.0"
}

func (v Version) IsZero() bool { return v.core == [3]uint64{} }

func (v Version) Compare(other Version) int {
	for i, n := range v.core {
		if n != other.core[i] {
			if n > other.core[i] {
				return 1
			}
			return -1
		}
	}
	if v.staging != other.staging {
		if v.staging {
			return -1
		}
		return 1
	}
	if v.rc > other.rc {
		return 1
	}
	if v.rc < other.rc {
		return -1
	}
	return 0
}
