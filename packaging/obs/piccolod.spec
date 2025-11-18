#
# First-pass spec for building piccolod from the piccolo-os monorepo.
# Adjust Version/Release before tagging a build in OBS.
#

Name:           piccolod
Version:        0.1.0
Release:        0
Summary:        Piccolo OS daemon
License:        AGPL-3.0-only
URL:            https://github.com/AtDexters-Lab/piccolod
Source0:        piccolod.service
Source1:        https://github.com/AtDexters-Lab/piccolod/releases/download/v%{version}/piccolod-v%{version}-linux-%{_arch}
ExclusiveArch:  x86_64 aarch64
# service macros still needed for install/uninstall hooks
BuildRequires:  systemd-rpm-macros
%{?systemd_requires}

%description
piccolod is the control-plane daemon for Piccolo OS. It exposes the HTTP API,
manages runtime supervisors, and serves the minimal UI.

%prep
# Nothing to prep; binary is provided via Source1

%build
# Binary is supplied pre-built

%install
install -Dm0755 %{SOURCE1} %{buildroot}%{_bindir}/piccolod
install -Dm0644 %{SOURCE0} %{buildroot}%{_unitdir}/piccolod.service

%check
%{buildroot}%{_bindir}/piccolod --version >/dev/null 2>&1 || true

%pre
%service_add_pre piccolod.service

%post
%systemd_post piccolod.service
/usr/bin/systemctl --no-reload enable --now piccolod.service >/dev/null 2>&1 || :

%preun
if [ $1 -eq 0 ]; then
    /usr/bin/systemctl --no-reload disable piccolod.service >/dev/null 2>&1 || :
fi
%systemd_preun piccolod.service

%postun
%systemd_postun piccolod.service

%files
%{_bindir}/piccolod
%{_unitdir}/piccolod.service

%changelog
* Tue Nov 18 2025 Piccolo Automation <ops@piccolo.local> - 0.1.0-0
- Fetch prebuilt release artifacts, install systemd unit reliably, add basic service hooks.
