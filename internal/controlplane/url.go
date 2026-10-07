package controlplane

import (
	"errors"
	"net/url"
	"strings"
)

func ValidateAPIURL(apiURL string) error {
	base, err := url.Parse(apiURL)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.ForceQuery || strings.Contains(apiURL, "#") {
		return errors.New("API URL must be an HTTPS base URL without credentials, query, or fragment")
	}
	loopback := base.Hostname() == "localhost" || base.Hostname() == "127.0.0.1" || base.Hostname() == "::1"
	if base.Scheme != "https" && !(base.Scheme == "http" && loopback) {
		return errors.New("API URL must use HTTPS (HTTP is allowed only on loopback for local development)")
	}
	return nil
}
