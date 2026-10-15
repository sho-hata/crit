package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sho-hata/crit/internal/daemon"
	"github.com/sho-hata/crit/internal/testutil"
)

func TestStopDaemonOnApproval(t *testing.T) {
	const noSessionFile = 0
	tests := []struct {
		name                string
		approved            bool
		cleanup             bool
		shutdownCode        int // HTTP status the daemon answers /api/shutdown with
		sessionPID          int
		successorDuringWait bool // a daemon registers under the key while this one exits
		exitUnconfirmed     bool
		wantRequest         bool
		wantSignal          bool
		wantWaited          bool
		wantRemoved         bool
	}{
		{name: "not approved", approved: false, cleanup: true, shutdownCode: http.StatusOK, sessionPID: os.Getpid()},
		{name: "daemon accepts", approved: true, cleanup: true, shutdownCode: http.StatusOK, sessionPID: os.Getpid(), wantRequest: true, wantWaited: true, wantRemoved: true},
		{name: "daemon survives forced kill", approved: true, cleanup: true, shutdownCode: http.StatusOK, sessionPID: os.Getpid(), exitUnconfirmed: true, wantRequest: true, wantWaited: true},
		{name: "daemon accepts, cleanup_on_approve off", approved: true, shutdownCode: http.StatusOK, sessionPID: os.Getpid(), wantRequest: true},
		{name: "successor registers while the daemon exits", approved: true, cleanup: true, shutdownCode: http.StatusOK, sessionPID: os.Getpid(), successorDuringWait: true, wantRequest: true, wantWaited: true},
		{name: "another daemon on the port", approved: true, cleanup: true, shutdownCode: http.StatusConflict, sessionPID: os.Getpid(), wantRequest: true},
		{name: "daemon already shutting down for another client", approved: true, cleanup: true, shutdownCode: http.StatusNotFound, sessionPID: noSessionFile, wantRequest: true, wantWaited: true, wantRemoved: true},
		{name: "session replaced by a newer daemon", approved: true, cleanup: true, shutdownCode: http.StatusNotFound, sessionPID: os.Getpid() + 1, wantRequest: true},
		{name: "older daemon without /api/shutdown", approved: true, cleanup: true, shutdownCode: http.StatusNotFound, sessionPID: os.Getpid(), wantRequest: true, wantSignal: true, wantWaited: true, wantRemoved: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testutil.SetHome(t, t.TempDir())

			var gotQuery string
			requested := false
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/shutdown" {
					http.NotFound(w, r)
					return
				}
				requested = true
				gotQuery = r.URL.RawQuery
				if tt.shutdownCode != http.StatusOK {
					http.Error(w, http.StatusText(tt.shutdownCode), tt.shutdownCode)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "stopping"})
			}))
			t.Cleanup(ts.Close)
			port, _ := strconv.Atoi(ts.URL[strings.LastIndex(ts.URL, ":")+1:])

			reviewPath := filepath.Join(t.TempDir(), "review")
			testutil.WriteFile(t, filepath.Join(reviewPath, "review.json"), "{}")
			entry := daemon.SessionEntry{PID: os.Getpid(), Port: port, ReviewPath: reviewPath}
			key := "approvaltest123"
			writeSession := func(pid int) {
				file := entry
				file.PID = pid
				if err := daemon.WriteSessionFile(key, file); err != nil {
					t.Fatalf("WriteSessionFile: %v", err)
				}
			}
			if tt.sessionPID != noSessionFile {
				writeSession(tt.sessionPID)
			}

			signalled, waited := false, false
			origTerminate, origWait, origGrace := terminateDaemonProcess, waitForDaemonExit, approvalSignalGrace
			terminateDaemonProcess = func(*os.Process) error { signalled = true; return nil }
			waitForDaemonExit = func(int) bool {
				waited = true
				if _, err := os.Stat(reviewPath); err != nil {
					t.Error("the review was removed before the daemon exited")
				}
				if tt.successorDuringWait {
					writeSession(os.Getpid() + 1)
				}
				return !tt.exitUnconfirmed
			}
			approvalSignalGrace = 0
			t.Cleanup(func() {
				terminateDaemonProcess, waitForDaemonExit, approvalSignalGrace = origTerminate, origWait, origGrace
			})

			stopDaemonOnApproval(tt.approved, entry, key, tt.cleanup)

			if requested != tt.wantRequest {
				t.Errorf("shutdown requested = %v, want %v", requested, tt.wantRequest)
			}
			if tt.wantRequest && gotQuery != "pid="+strconv.Itoa(os.Getpid()) {
				t.Errorf("query = %q, want the daemon's pid", gotQuery)
			}
			if signalled != tt.wantSignal {
				t.Errorf("signalled = %v, want %v", signalled, tt.wantSignal)
			}
			if waited != tt.wantWaited {
				t.Errorf("waited for the daemon to exit = %v, want %v", waited, tt.wantWaited)
			}
			_, statErr := os.Stat(reviewPath)
			if removed := os.IsNotExist(statErr); removed != tt.wantRemoved {
				t.Errorf("review removed by the client = %v, want %v", removed, tt.wantRemoved)
			}
		})
	}
}
