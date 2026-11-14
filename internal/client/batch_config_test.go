package client

import (
	"os"
	"testing"
)

func TestBatchUploadDefaultsOn(t *testing.T) {
	withIsolatedConfig(t)
	if !LoadConfig().BatchUploadEnabled() {
		t.Fatal("batch upload should be on by default")
	}
}

func TestBatchUploadFromFileAndEnv(t *testing.T) {
	withIsolatedConfig(t)
	off := false
	if err := SaveFileConfig(&FileConfig{BatchUpload: &off}); err != nil {
		t.Fatal(err)
	}
	if LoadConfig().BatchUploadEnabled() {
		t.Fatal("batch_upload:false in the file should disable batching")
	}
	_ = os.Setenv(EnvBatchUpload, "1")
	if !LoadConfig().BatchUploadEnabled() {
		t.Fatal("PROMPTCONDUIT_BATCH_UPLOAD=1 should override the file")
	}
	_ = os.Setenv(EnvBatchUpload, "0")
	if err := SaveFileConfig(&FileConfig{}); err != nil {
		t.Fatal(err)
	}
	if LoadConfig().BatchUploadEnabled() {
		t.Fatal("PROMPTCONDUIT_BATCH_UPLOAD=0 should disable batching")
	}
}

func TestBatchUploadGlobalAppliesToActiveEnv(t *testing.T) {
	withIsolatedConfig(t)
	off := false
	fc := &FileConfig{
		CurrentEnv:   "prod",
		Environments: map[string]*Config{"prod": {APIKey: "k", APIURL: "https://x"}},
		BatchUpload:  &off,
	}
	if err := SaveFileConfig(fc); err != nil {
		t.Fatal(err)
	}
	if LoadConfig().BatchUploadEnabled() {
		t.Fatal("a top-level batch_upload:false is a global default for every env")
	}
}
