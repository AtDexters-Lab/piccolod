import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:piccolo_os/core/models/wifi_models.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/network/widgets/wifi_status_card.dart';

void main() {
  testWidgets('Ethernet-only status remains useful without WiFi actions', (
    tester,
  ) async {
    await tester.pumpWidget(
      MaterialApp(
        home: Scaffold(
          body: WifiStatusCard(
            status: networkStatus(
              activeUplink: 'ethernet',
              activeUplinkIface: 'enp2s0',
              connectivity: 'full',
              wifiAvailable: false,
            ),
            onForgetNetwork: () {},
            onScanNetworks: () {},
          ),
        ),
      ),
    );

    expect(find.text('Connected via Ethernet'), findsOneWidget);
    expect(find.text('Interface'), findsOneWidget);
    expect(find.text('enp2s0'), findsOneWidget);
    expect(find.text('Connect to WiFi'), findsNothing);
    expect(find.text('Change Network'), findsNothing);
    expect(find.text('Forget Network'), findsNothing);
  });

  testWidgets('status card distinguishes limited, portal, unknown, and AP', (
    tester,
  ) async {
    final cases = <(NetworkStatus, String)>[
      (
        networkStatus(
          activeUplink: 'ethernet',
          activeUplinkIface: 'enp2s0',
          connectivity: 'limited',
          wifiAvailable: false,
        ),
        'Connected (limited)',
      ),
      (
        networkStatus(
          activeUplink: 'wifi',
          activeUplinkIface: 'wlan0',
          connectivity: 'portal',
        ),
        'Sign-in required',
      ),
      (
        networkStatus(activeUplink: 'none', connectivity: 'unknown'),
        'Checking network',
      ),
      (
        networkStatus(
          activeUplink: 'none',
          connectivity: 'unknown',
          apActive: true,
        ),
        'Setup Mode',
      ),
    ];

    for (final (status, label) in cases) {
      await tester.pumpWidget(
        MaterialApp(
          home: Scaffold(
            body: WifiStatusCard(
              status: status,
              onForgetNetwork: () {},
              onScanNetworks: () {},
            ),
          ),
        ),
      );
      expect(find.text(label), findsOneWidget);
    }
  });
}

NetworkStatus networkStatus({
  required String activeUplink,
  required String connectivity,
  String? activeUplinkIface,
  bool wifiAvailable = true,
  bool apActive = false,
}) {
  return NetworkStatus(
    available: wifiAvailable,
    activeUplink: activeUplink,
    activeUplinkIface: activeUplinkIface,
    connectivity: connectivity,
    interfaces: const [],
    apActive: apActive,
    hasSavedNetwork: false,
  );
}
