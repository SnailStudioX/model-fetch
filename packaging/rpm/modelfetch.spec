# ModelFetch - HuggingFace Model Downloader
# RPM Spec for Linux desktop integration
#
# Build:
#   rpmbuild -ba modelfetch.spec \
#     --define "_sourcedir $(pwd)" \
#     --define "_rpmdir $(pwd)/RPMS"
#
# Requires the following files in _sourcedir:
#   modelfetch_linux_amd64  — pre-built binary (Source0)
#   modelfetch.desktop      — Freedesktop entry (Source1)
#   modelfetch.svg          — scalable icon (Source2)
#   LICENSE             — Apache-2.0 license (Source3)
#   README.md           — project readme (Source4)

%global appname modelfetch
%global appdesc HuggingFace Model Downloader
%{!?appversion:%global appversion 0.0.1}

Name:           %{appname}
Version:        %{appversion}
Release:        1%{?dist}
Summary:        Desktop-style web UI for HuggingFace model downloads

License:        Apache-2.0
URL:            https://github.com/bashrusakh/modelfetch
Source0:        modelfetch_linux_amd64
Source1:        modelfetch.desktop
Source2:        modelfetch.svg
Source3:        LICENSE
Source4:        README.md

BuildArch:      x86_64
BuildRoot:      %{_tmppath}/%{name}-%{version}-%{release}-root

Requires:       ca-certificates

%description
ModelFetch is a local web dashboard for searching, analyzing, downloading, and
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
install -D -m 0755 modelfetch_linux_amd64 %{buildroot}%{_bindir}/modelfetch

# Desktop file
install -D -m 0644 modelfetch.desktop %{buildroot}%{_datadir}/applications/modelfetch.desktop

# Icon (scalable SVG)
install -D -m 0644 modelfetch.svg %{buildroot}%{_datadir}/icons/hicolor/scalable/apps/modelfetch.svg

%clean
rm -rf %{buildroot}

%files
%defattr(-,root,root,-)
%{_bindir}/modelfetch
%{_datadir}/applications/modelfetch.desktop
%{_datadir}/icons/hicolor/scalable/apps/modelfetch.svg

%license LICENSE
%doc README.md

%changelog
* Mon Jun 29 2026 ModelFetch Maintainer <https://github.com/bashrusakh/modelfetch> - 0.0.1-1
- Initial RPM package with desktop integration
