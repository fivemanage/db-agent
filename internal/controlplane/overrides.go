package controlplane

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/fivemanage/db-agent/internal/config"
)

func responseOverrides(contents []byte) (*config.DashboardOverrides, error) {
	var envelope struct {
		Config json.RawMessage `json:"config_overrides"`
	}
	if json.Unmarshal(contents, &envelope) != nil {
		return nil, errors.New("invalid control-plane response")
	}
	if len(envelope.Config) == 0 || string(envelope.Config) == "null" {
		return nil, nil
	}
	overrides, err := config.DecodeDashboardOverrides(envelope.Config)
	if err != nil {
		return nil, err
	}
	return &overrides, nil
}

func (c *Client) applyOverrides(ctx context.Context, contents []byte) error {
	overrides, err := responseOverrides(contents)
	if err != nil {
		return err
	}
	if overrides != nil && c.OnOverrides != nil {
		if err := c.OnOverrides(ctx, *overrides); err != nil {
			return errors.New("cannot apply dashboard configuration safely; check local config permissions and logs before restarting")
		}
	}
	return nil
}
