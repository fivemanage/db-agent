package cmd

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/spf13/cobra"
)

func TestDiagnosticCommandFailsDespiteAPIReachability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveEnrollment(path, server.URL, "dac_never-print-this"); err != nil {
		t.Fatal(err)
	}
	old := configPath
	configPath = path
	t.Cleanup(func() { configPath = old })
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetContext(context.Background())
	command.SetOut(&output)
	err := testCmd.RunE(command, nil)
	if err == nil || !strings.Contains(output.String(), "+ Control-plane reachability:") || !strings.Contains(output.String(), "x Database connectivity:") {
		t.Fatalf("diagnostic result: err=%v output=%s", err, output.String())
	}
	if strings.Contains(output.String(), "never-print-this") {
		t.Fatal("agent credential was printed")
	}
}
