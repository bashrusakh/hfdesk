// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/service"
)

// TestTrayServiceItemsAskTheSameRegisteredServiceDialog proves that the
// Service... item and the install item reach the same registered-service
// dialog: both read the registration again, both build the gate's question
// from that read, and both reach it through offerRegisteredServiceFromTray —
// so the two menu items can never ask different questions or act on
// different readings of the machine.
//
// The dialog answers Cancel in both calls, so nothing is launched, no
// elevation is requested and no browser opens beside the dialog.
func TestTrayServiceItemsAskTheSameRegisteredServiceDialog(t *testing.T) {
	freshServiceInstallGuard(t)
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return runningServiceStatus(service.StateStopped, "127.0.0.1:18080"), nil
	})
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})
	replaceSeam(t, &confirmServiceInstall, func(string) bool { return true })

	var prompts []serviceChoicePrompt
	replaceSeam(t, &showServiceChoice, func(prompt serviceChoicePrompt) serviceChoice {
		prompts = append(prompts, prompt)
		return serviceChoiceCancel
	})
	replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
		t.Fatal("a cancelled service choice launched an elevated helper")
		return 0, nil
	})
	replaceSeam(t, &launchElevatedProcess, func(elevatedLaunch) (*elevatedProcess, error) {
		t.Fatal("a cancelled service choice started a handover")
		return nil, nil
	})
	replaceSeam(t, &openBrowser, func(string) error {
		t.Fatal("a cancelled service choice opened a browser")
		return nil
	})
	messages := recordMessages(t)

	openRegisteredServiceFromTray(testGateSingleton)
	if handover := installServiceFromTray(testGateSingleton); handover != nil {
		handover.close()
		t.Fatal("the install item handed the installation over although a registration exists")
	}

	if len(prompts) != 2 {
		t.Fatalf("the two menu items asked %d questions, want the same dialog once each: %+v",
			len(prompts), prompts)
	}
	if prompts[0] != prompts[1] {
		t.Errorf("the Service... item asked %+v, the install item asked %+v: the two items must ask the same question",
			prompts[0], prompts[1])
	}
	if !servicePromptStopped(prompts[0]) {
		t.Errorf("the dialog was not asked about the positively confirmed stopped service: %+v", prompts[0])
	}
	if prompts[0].URL != "http://127.0.0.1:18080" {
		t.Errorf("the dialog named the address %q, want the registered one", prompts[0].URL)
	}
	if len(*messages) != 0 {
		t.Errorf("the two items showed %v, want the dialog to be the only report", *messages)
	}
}

// TestTrayServiceItemChangesNothingWithoutARegistration proves that the
// Service... item on a stale menu line — the registration is gone between
// the last status read and the click — changes nothing: it opens no dialog
// and shows no notice, and the status line catches up with the menu's own
// refresh instead.
func TestTrayServiceItemChangesNothingWithoutARegistration(t *testing.T) {
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return service.Status{State: service.StateNotInstalled}, nil
	})
	replaceSeam(t, &showServiceChoice, func(serviceChoicePrompt) serviceChoice {
		t.Fatal("the Service... item asked a question although no registration exists")
		return serviceChoiceCancel
	})
	replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
		t.Fatal("the Service... item launched an elevated helper although no registration exists")
		return 0, nil
	})
	messages := recordMessages(t)

	openRegisteredServiceFromTray(testGateSingleton)

	if len(*messages) != 0 {
		t.Errorf("the Service... item showed %v, want no report for a machine without a registration", *messages)
	}
}

// TestTrayServiceItemReportsAStateItCannotDecideOn proves that a
// registration the host cannot decide on reaches the notice of the gate
// instead of a dialog built on a state that was never settled: both the
// pending transition and the failed read are reported exactly once, with
// the caption of the gate.
func TestTrayServiceItemReportsAStateItCannotDecideOn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  service.Status
		readErr error
		caption string
	}{
		{
			name:    "pending transition",
			status:  service.Status{State: service.StateStopping},
			readErr: errServiceGatePending,
			caption: serviceGatePendingCaption,
		},
		{
			name:    "unreadable registration",
			status:  service.Status{},
			readErr: errors.New("the service control manager did not answer"),
			caption: serviceGateUnreadableCaption,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The gate waits for a pending state to settle before it
			// reports it, so the budget of the read is shortened for
			// the wait of the test.
			replaceSeam(t, &serviceStatusTimeout, 50*time.Millisecond)
			replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
				return tc.status, tc.readErr
			})
			replaceSeam(t, &showServiceChoice, func(serviceChoicePrompt) serviceChoice {
				t.Fatal("the Service... item asked a question about a state it could not decide on")
				return serviceChoiceCancel
			})
			messages := recordMessages(t)

			openRegisteredServiceFromTray(testGateSingleton)

			if len(*messages) != 1 {
				t.Fatalf("the Service... item showed %v, want the one notice of the state", *messages)
			}
			if (*messages)[0].caption != tc.caption {
				t.Errorf("the caption is %q, want %q", (*messages)[0].caption, tc.caption)
			}
		})
	}
}
