package client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"net/url"
	"strings"
	"unicode"

	"github.com/michael-bill/knotra/internal/protocol"
)

// A JSON object alone is not proof of command acceptance. Validate the durable
// identity in the route's receipt before replacing the pending journal entry.
func validateReceipt(route string, data []byte) error {
	var receipt struct {
		Definition *struct {
			ID string `json:"id"`
		} `json:"definition"`
		Run *struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"run"`
		Artifact *struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			MediaType string `json:"mediaType"`
			Size      *int64 `json:"size"`
			SHA256    string `json:"sha256"`
			Path      string `json:"path"`
		} `json:"artifact"`
		Accepted  bool   `json:"accepted"`
		RunID     string `json:"runId"`
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(data, &receipt); err != nil {
		return err
	}
	valid := false

	switch route {
	case "/definitions":
		valid = receipt.Definition != nil && opaqueID(receipt.Definition.ID)
	case "/runs":
		if receipt.Run != nil && opaqueID(receipt.Run.ID) {
			switch receipt.Run.Status {
			case "pending", "ready", "running", "retry_wait", "waiting_human", "waiting_resolution", "succeeded", "skipped", "failed", "cancelled":
				valid = true
			}
		}
	case "/artifacts":
		if a := receipt.Artifact; a != nil {
			_, _, mediaErr := mime.ParseMediaType(a.MediaType)
			digest, digestErr := hex.DecodeString(a.SHA256)
			valid = opaqueID(a.ID) && a.Name != "" && mediaErr == nil && strings.Contains(a.MediaType, "/") && a.Size != nil && *a.Size >= 0 && *a.Size <= 64<<20 && digestErr == nil && len(digest) == sha256.Size && a.Path == ""
		}
	default:
		parts := strings.Split(strings.TrimPrefix(route, "/"), "/")
		if len(parts) >= 3 {
			id, err := url.PathUnescape(parts[1])
			if err == nil && opaqueID(id) && receipt.Accepted {
				if parts[0] == "requests" && len(parts) == 3 && parts[2] == "response" {
					valid = receipt.RequestID == id
				} else if parts[0] == "runs" && ((len(parts) == 3 && (parts[2] == "cancel" || parts[2] == "resume")) || (len(parts) == 5 && parts[2] == "instances" && parts[4] == "resolve")) {
					valid = receipt.RunID == id
				}
			}
		}
	}

	if !valid {
		return errors.New("receipt does not identify the accepted command result")
	}
	return nil
}

func opaqueID(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}

	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}

	return true
}

func validateErrorReceipt(data []byte, out *protocol.Error) error {
	if err := json.Unmarshal(data, out); err != nil {
		return err
	}
	if strings.TrimSpace(out.Code) == "" || strings.TrimSpace(out.Message) == "" || out.Diagnostics == nil {
		return errors.New("error receipt requires code, message and diagnostics")
	}
	return nil
}
