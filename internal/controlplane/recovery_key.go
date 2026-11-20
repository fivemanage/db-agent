package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/fivemanage/db-agent/internal/key"
)

const recoveryKeyPath = "/api/v1/agent/recovery-key"

type RecoveryKeyStatus string

const (
	RecoveryKeyDisabled RecoveryKeyStatus = "disabled"
	RecoveryKeyPending  RecoveryKeyStatus = "pending"
	RecoveryKeySaved    RecoveryKeyStatus = "saved"
)

type recoveryKeyResponse struct {
	Status RecoveryKeyStatus `json:"status"`
}

func (c *Client) RecoveryKeyStatus(ctx context.Context) (RecoveryKeyStatus, error) {
	data, err := c.request(ctx, request{
		operation: "managed recovery key status", method: http.MethodGet, path: recoveryKeyPath, credential: c.credential,
	})
	if err != nil {
		return "", err
	}
	var result recoveryKeyResponse
	if json.Unmarshal(data, &result) != nil {
		return "", errors.New("invalid managed recovery key status")
	}
	switch result.Status {
	case RecoveryKeyDisabled, RecoveryKeyPending, RecoveryKeySaved:
		return result.Status, nil
	default:
		return "", errors.New("invalid managed recovery key status")
	}
}

func (c *Client) UploadRecoveryKey(ctx context.Context, recoveryKey key.Key) error {
	body := struct {
		Key string `json:"key"`
	}{Key: recoveryKey.RecoveryString()}
	data, err := c.request(ctx, request{
		operation: "managed recovery key upload", method: http.MethodPost, path: recoveryKeyPath, credential: c.credential, body: body,
	})
	if err != nil {
		return err
	}
	var result recoveryKeyResponse
	if json.Unmarshal(data, &result) != nil || result.Status != RecoveryKeySaved {
		return errors.New("managed recovery key storage was not confirmed")
	}
	return nil
}
