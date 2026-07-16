import 'package:flutter_test/flutter_test.dart';
import 'package:piccolo_os/core/models/wifi_models.dart';

void main() {
  test('network status uses the concrete active interface', () {
    final status = NetworkStatus.fromJson({
      'active_uplink': 'ethernet',
      'active_uplink_iface': 'enp2s0',
      'connectivity': 'full',
      'interfaces': [
        {
          'kind': 'ethernet',
          'iface': 'enp1s0',
          'role': 'not_connected',
          'link_up': false,
          'has_ip': false,
        },
        {
          'kind': 'ethernet',
          'iface': 'enp2s0',
          'role': 'wan_lan',
          'link_up': true,
          'has_ip': true,
          'ipv4': ['10.42.0.38'],
        },
      ],
      'ap_active': false,
      'wifi_available': false,
      'has_saved_network': false,
    });

    expect(status.isEthernet, isTrue);
    expect(status.activeUplinkIface, 'enp2s0');
    expect(status.interfaces, hasLength(2));
    expect(status.interfaces.last.ipv4, ['10.42.0.38']);
  });

  test('presentation derives reconnecting without a backend state enum', () {
    final status = NetworkStatus.fromJson({
      'active_uplink': 'none',
      'connectivity': 'none',
      'interfaces': const <Object>[],
      'ap_active': false,
      'wifi_available': true,
      'has_saved_network': true,
      'saved_ssid': 'Home',
    });

    expect(status.isReconnecting, isTrue);
    expect(status.isDisconnected, isFalse);
  });

  test('limited connectivity remains connected but visibly qualified', () {
    final status = NetworkStatus.fromJson({
      'active_uplink': 'ethernet',
      'active_uplink_iface': 'enp2s0',
      'connectivity': 'limited',
      'interfaces': const <Object>[],
      'ap_active': false,
      'wifi_available': false,
      'has_saved_network': false,
    });

    expect(status.isEthernet, isTrue);
    expect(status.isLimitedConnectivity, isTrue);
    expect(status.isDisconnected, isFalse);
  });

  test('unknown connectivity never collapses into reconnecting', () {
    final status = NetworkStatus.fromJson({
      'active_uplink': 'none',
      'connectivity': 'unknown',
      'interfaces': const <Object>[],
      'ap_active': false,
      'wifi_available': true,
      'has_saved_network': true,
      'saved_ssid': 'Home',
    });

    expect(status.isUnknown, isTrue);
    expect(status.isReconnecting, isFalse);
    expect(status.isDisconnected, isFalse);
  });

  test('portal connectivity never collapses into reconnecting', () {
    final status = NetworkStatus.fromJson({
      'active_uplink': 'wifi',
      'active_uplink_iface': 'wlan0',
      'connectivity': 'portal',
      'interfaces': const <Object>[],
      'ap_active': false,
      'wifi_available': true,
      'has_saved_network': true,
      'saved_ssid': 'Hotel',
    });

    expect(status.isPortal, isTrue);
    expect(status.isReconnecting, isFalse);
    expect(status.isDisconnected, isFalse);
  });

  test(
    'an active route without usable connectivity stays visible as offline',
    () {
      final status = NetworkStatus.fromJson({
        'active_uplink': 'ethernet',
        'active_uplink_iface': 'enp2s0',
        'connectivity': 'none',
        'interfaces': const <Object>[],
        'ap_active': false,
        'wifi_available': false,
        'has_saved_network': false,
      });

      expect(status.isEthernet, isFalse);
      expect(status.isDisconnected, isTrue);
    },
  );

  test('access point presentation takes precedence over an active route', () {
    final status = NetworkStatus.fromJson({
      'active_uplink': 'ethernet',
      'connectivity': 'full',
      'interfaces': const <Object>[],
      'ap_active': true,
      'wifi_available': true,
      'has_saved_network': false,
    });

    expect(status.isAPMode, isTrue);
    expect(status.isEthernet, isFalse);
    expect(status.isLimitedConnectivity, isFalse);
    expect(status.isPortal, isFalse);
    expect(status.isUnknown, isFalse);
  });
}
