# HFDesk - HuggingFace Model Downloader
# RPM Spec for Linux desktop integration
#
# Build:
#   rpmbuild -ba hfdesk.spec \
#     --define "_sourcedir $(pwd)" \
#     --define "_rpmdir $(pwd)/RPMS"
#
# Requires: the pre-built hfdesk_linux_amd64 binary in _sourcedir.

%global appname hfdesk
%global appdesc HuggingFace Model Downloader
%global appversion %{!?appversion:1.2.1}

Name:           %{appname}
Version:        %{appversion}
Release:        1%{?dist}
Summary:        Desktop-style web UI for HuggingFace model downloads

License:        Apache-2.0
URL:            https://github.com/bashrusakh/hfdesk
Source0:        hfdesk_linux_amd64
Source1:        hfdesk.desktop
Source2:        hfdesk.svg
Source3:        LICENSE
Source4:        README.md

BuildArch:      x86_64
BuildRoot:      %{_tmppath}/%{name}-%{version}-%{release}-root

Requires:       ca-certificates

%description
HFDesk is a local web dashboard for searching, analyzing, downloading, and
managing HuggingFace models and datasets. It runs as a small Go HTTP server
with an embedded web UI and supports resumable downloads, HF cache layout,
LM Studio-style local folders, job tracking, cache browsing, and mirror
operations.

%prep
%setup -q -T -c %{name}-%{version}
cp %{SOURCE0} .
cp %{SOURCE1} .
cp %{SOURCE2} .
cp %{SOURCE3} .
cp %{SOURCE4} .

%build
# Binary is pre-built; nothing to compile.

%install
rm -rf %{buildroot}

# Binary
install -D -m 0755 hfdesk_linux_amd64 %{buildroot}%{_bindir}/hfdesk

# Desktop file
install -D -m 0644 hfdesk.desktop %{buildroot}%{_datadir}/applications/hfdesk.desktop

# Icon (scalable SVG)
install -D -m 0644 hfdesk.svg %{buildroot}%{_datadir}/icons/hicolor/scalable/apps/hfdesk.svg

%clean
rm -rf %{buildroot}

%files
%defattr(-,root,root,-)
%{_bindir}/hfdesk
%{_datadir}/applications/hfdesk.desktop
%{_datadir}/icons/hicolor/scalable/apps/hfdesk.svg

%license LICENSE
%doc README.md

%changelog
* Mon Jun 29 2026 HFDesk Maintainer <https://github.com/bashrusakh/hfdesk> - 1.2.1-1
- Initial RPM package with desktop integration
