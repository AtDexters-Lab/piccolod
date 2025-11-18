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

%post
%systemd_post piccolod.service

%preun
%systemd_preun piccolod.service

%postun
%systemd_postun piccolod.service

%files
%{_bindir}/piccolod
%{_unitdir}/piccolod.service
