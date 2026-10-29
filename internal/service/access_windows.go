// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const scmChangeNotifyPrivilege = "SeChangeNotifyPrivilege"

// FILE_ALL_ACCESS is not exported by x/sys/windows.
const scmFileAllAccess uint32 = 0x001F01FF

type scmPathAccess interface {
	Check(path string, desiredAccess uint32) (bool, error)
	HasEnabledPrivilege(name string) (bool, error)
	Close()
}

var scmOpenPathAccess = scmOpenServicePathAccess

type scmServicePathAccess struct {
	manager windows.Handle
	service windows.Handle
	process windows.Handle
	token   windows.Token
}

// scmOpenServicePathAccess obtains the effective token of the running HFDesk
// service. The open service and process handles are retained through the
// preflight; the SCM process id is checked again after opening its token so a
// reused pid or a replaced registration cannot supply a different identity.
func scmOpenServicePathAccess(expectedProcessID uint32) (scmPathAccess, error) {
	if expectedProcessID == 0 {
		return nil, nil
	}
	access := &scmServicePathAccess{}
	fail := func(err error) (scmPathAccess, error) {
		access.Close()
		return nil, err
	}
	var err error
	access.manager, err = windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return fail(err)
	}
	name, err := windows.UTF16PtrFromString(ServiceName)
	if err != nil {
		return fail(err)
	}
	access.service, err = windows.OpenService(access.manager, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return fail(err)
	}
	status, err := scmQueryNativeServiceStatus(access.service)
	if err != nil {
		return fail(err)
	}
	if status.CurrentState != windows.SERVICE_RUNNING || status.ProcessId != expectedProcessID {
		return fail(errors.New("the service process changed before its access token could be read"))
	}
	access.process, err = windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, expectedProcessID)
	if err != nil {
		return fail(err)
	}
	var primary windows.Token
	if err := windows.OpenProcessToken(access.process, windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &primary); err != nil {
		return fail(err)
	}
	defer primary.Close()
	user, err := primary.GetTokenUser()
	if err != nil {
		return fail(err)
	}
	if user.User.Sid == nil || user.User.Sid.String() != scmServiceIdentitySID {
		return fail(errors.New("the running service process does not carry the LocalService identity"))
	}
	if err := windows.DuplicateTokenEx(primary, windows.TOKEN_QUERY, nil, windows.SecurityImpersonation, windows.TokenImpersonation, &access.token); err != nil {
		return fail(err)
	}
	status, err = scmQueryNativeServiceStatus(access.service)
	if err != nil {
		return fail(err)
	}
	if status.CurrentState != windows.SERVICE_RUNNING || status.ProcessId != expectedProcessID {
		return fail(errors.New("the service process changed while its access token was read"))
	}
	return access, nil
}

func (a *scmServicePathAccess) Close() {
	if a == nil {
		return
	}
	if a.token != 0 {
		_ = a.token.Close()
		a.token = 0
	}
	if a.process != 0 {
		_ = windows.CloseHandle(a.process)
		a.process = 0
	}
	if a.service != 0 {
		_ = windows.CloseServiceHandle(a.service)
		a.service = 0
	}
	if a.manager != 0 {
		_ = windows.CloseServiceHandle(a.manager)
		a.manager = 0
	}
}

func scmQueryNativeServiceStatus(handle windows.Handle) (windows.SERVICE_STATUS_PROCESS, error) {
	var status windows.SERVICE_STATUS_PROCESS
	var needed uint32
	err := windows.QueryServiceStatusEx(handle, windows.SC_STATUS_PROCESS_INFO,
		(*byte)(unsafe.Pointer(&status)), uint32(unsafe.Sizeof(status)), &needed)
	return status, err
}

func (a *scmServicePathAccess) Check(path string, desiredAccess uint32) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	return scmAccessCheckDescriptor(a.token, sd, desiredAccess)
}

func (a *scmServicePathAccess) HasEnabledPrivilege(name string) (bool, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false, err
	}
	var wanted windows.LUID
	if err := windows.LookupPrivilegeValue(nil, namePtr, &wanted); err != nil {
		return false, err
	}
	var needed uint32
	err = windows.GetTokenInformation(a.token, windows.TokenPrivileges, nil, 0, &needed)
	if err == nil && needed == 0 {
		return false, nil
	}
	if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || needed == 0 {
		return false, errors.New("the service token privilege list could not be sized")
	}
	buffer := make([]byte, needed)
	if err := windows.GetTokenInformation(a.token, windows.TokenPrivileges, &buffer[0], uint32(len(buffer)), &needed); err != nil {
		return false, err
	}
	privileges := (*windows.Tokenprivileges)(unsafe.Pointer(&buffer[0]))
	for _, privilege := range privileges.AllPrivileges() {
		if privilege.Luid == wanted {
			return privilege.Attributes&windows.SE_PRIVILEGE_ENABLED != 0, nil
		}
	}
	return false, nil
}

type scmGenericMapping struct {
	GenericRead    uint32
	GenericWrite   uint32
	GenericExecute uint32
	GenericAll     uint32
}

var scmAccessCheckProc = windows.NewLazySystemDLL("advapi32.dll").NewProc("AccessCheck")

func scmAccessCheckDescriptor(token windows.Token, sd *windows.SECURITY_DESCRIPTOR, desiredAccess uint32) (bool, error) {
	if err := scmAccessCheckProc.Find(); err != nil {
		return false, err
	}
	mapping := scmGenericMapping{
		GenericRead:    windows.FILE_GENERIC_READ,
		GenericWrite:   windows.FILE_GENERIC_WRITE,
		GenericExecute: windows.FILE_GENERIC_EXECUTE,
		GenericAll:     scmFileAllAccess,
	}
	desiredAccess = scmMapGenericAccess(desiredAccess, mapping)
	privilegeSet := make([]byte, 1024)
	privilegeSetLength := uint32(len(privilegeSet))
	var grantedAccess uint32
	var accessStatus int32
	r1, _, callErr := scmAccessCheckProc.Call(
		uintptr(unsafe.Pointer(sd)),
		uintptr(token),
		uintptr(desiredAccess),
		uintptr(unsafe.Pointer(&mapping)),
		uintptr(unsafe.Pointer(&privilegeSet[0])),
		uintptr(unsafe.Pointer(&privilegeSetLength)),
		uintptr(unsafe.Pointer(&grantedAccess)),
		uintptr(unsafe.Pointer(&accessStatus)),
	)
	if r1 == 0 {
		if callErr != syscall.Errno(0) {
			return false, callErr
		}
		return false, errors.New("AccessCheck failed without a Windows error")
	}
	return accessStatus != 0, nil
}

func scmMapGenericAccess(desired uint32, mapping scmGenericMapping) uint32 {
	if desired&windows.GENERIC_READ != 0 {
		desired = desired&^windows.GENERIC_READ | mapping.GenericRead
	}
	if desired&windows.GENERIC_WRITE != 0 {
		desired = desired&^windows.GENERIC_WRITE | mapping.GenericWrite
	}
	if desired&windows.GENERIC_EXECUTE != 0 {
		desired = desired&^windows.GENERIC_EXECUTE | mapping.GenericExecute
	}
	if desired&windows.GENERIC_ALL != 0 {
		desired = desired&^windows.GENERIC_ALL | mapping.GenericAll
	}
	return desired
}

func scmCheckPathAccess(path string, desiredAccess uint32, access scmPathAccess) (scmExecutableAccess, string) {
	if access == nil {
		return scmExecutableAccessInconclusive, "the running service token is unavailable"
	}
	resolved, err := scmResolvePath(path)
	if err != nil {
		return scmExecutableAccessInconclusive, fmt.Sprintf("the real location of %q cannot be read: %v", path, err)
	}
	allowed, err := access.Check(resolved, desiredAccess)
	if err != nil {
		return scmExecutableAccessInconclusive, fmt.Sprintf("AccessCheck for %q could not be completed: %v", resolved, err)
	}
	if !allowed {
		return scmExecutableAccessDenied, fmt.Sprintf("Windows AccessCheck denied the requested access to %q", resolved)
	}
	bypass, err := access.HasEnabledPrivilege(scmChangeNotifyPrivilege)
	if err != nil {
		return scmExecutableAccessInconclusive, fmt.Sprintf("the enabled state of %s could not be read: %v", scmChangeNotifyPrivilege, err)
	}
	if bypass {
		return scmExecutableAccessAllowed, ""
	}
	for _, dir := range scmParentDirs(resolved) {
		allowed, err := access.Check(dir, windows.FILE_TRAVERSE)
		if err != nil {
			return scmExecutableAccessInconclusive, fmt.Sprintf("AccessCheck for traversal of %q could not be completed: %v", dir, err)
		}
		if !allowed {
			return scmExecutableAccessDenied, fmt.Sprintf("Windows AccessCheck denied traversal of %q", dir)
		}
	}
	return scmExecutableAccessAllowed, ""
}
