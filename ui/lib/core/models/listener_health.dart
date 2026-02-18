import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Shared visual mappings for listener health status.
class ListenerHealthVisuals {
  ListenerHealthVisuals._();

  static Color colorForStatus(String status) {
    switch (status) {
      case 'degraded':
      case 'recovering':
        return PiccoloTheme.warning;
      case 'error':
        return PiccoloTheme.critical;
      default:
        return PiccoloTheme.inkMuted;
    }
  }

  static Color backgroundForStatus(String status) {
    switch (status) {
      case 'degraded':
      case 'recovering':
        return PiccoloTheme.warning.withValues(alpha: 0.1);
      case 'error':
        return PiccoloTheme.critical.withValues(alpha: 0.1);
      default:
        return PiccoloTheme.mist;
    }
  }

  static IconData iconForStatus(String status) {
    switch (status) {
      case 'degraded':
        return PiccoloIcons.warning;
      case 'recovering':
        return PiccoloIcons.hourglass;
      case 'error':
        return PiccoloIcons.error;
      default:
        return PiccoloIcons.info;
    }
  }
}

class ListenerHealth {

  const ListenerHealth({
    required this.status,
    this.reasonCode = '',
    this.reason = '',
    this.details,
    this.recoveryEta,
    this.recoverable = true,
    this.actionRequired = false,
    this.certStatuses,
    this.lastChecked,
    this.lastOk,
  });

  factory ListenerHealth.fromJson(Map<String, dynamic> json) {
    Map<String, CertHealthStatus>? certs;
    final rawCerts = json['cert_statuses'];
    if (rawCerts is Map) {
      certs = rawCerts.map(
        (k, v) => MapEntry(
          k.toString(),
          v is Map
              ? CertHealthStatus.fromJson(Map<String, dynamic>.from(v))
              : const CertHealthStatus(status: 'unknown'),
        ),
      );
    }

    return ListenerHealth(
      status: (json['status'] ?? 'unknown').toString(),
      reasonCode: (json['reason_code'] ?? '').toString(),
      reason: (json['reason'] ?? '').toString(),
      details: json['details']?.toString(),
      recoveryEta: json['recovery_eta']?.toString(),
      recoverable: json['recoverable'] == true,
      actionRequired: json['action_required'] == true,
      certStatuses: certs,
      lastChecked: json['last_checked']?.toString(),
      lastOk: json['last_ok']?.toString(),
    );
  }
  final String status;
  final String reasonCode;
  final String reason;
  final String? details;
  final String? recoveryEta;
  final bool recoverable;
  final bool actionRequired;
  final Map<String, CertHealthStatus>? certStatuses;
  final String? lastChecked;
  final String? lastOk;

  bool get isOk => status == 'ok';
  bool get isRecovering => status == 'recovering';
  bool get isError => status == 'error';
  bool get isDegraded => status == 'degraded';
}

class CertHealthStatus {

  const CertHealthStatus({
    required this.status,
    this.reasonCode = '',
    this.recoveryEta,
  });

  factory CertHealthStatus.fromJson(Map<String, dynamic> json) {
    return CertHealthStatus(
      status: (json['status'] ?? 'unknown').toString(),
      reasonCode: (json['reason_code'] ?? '').toString(),
      recoveryEta: json['recovery_eta']?.toString(),
    );
  }
  final String status;
  final String reasonCode;
  final String? recoveryEta;
}

class ListenerHealthEvent {

  const ListenerHealthEvent({
    required this.app,
    required this.listener,
    required this.health,
    this.timestamp,
  });

  factory ListenerHealthEvent.fromJson(Map<String, dynamic> json) {
    return ListenerHealthEvent(
      app: (json['app'] ?? '').toString(),
      listener: (json['listener'] ?? '').toString(),
      health: ListenerHealth.fromJson(
        json['health'] is Map<dynamic, dynamic>
            ? Map<String, dynamic>.from(json['health'] as Map<dynamic, dynamic>)
            : <String, dynamic>{},
      ),
      timestamp: json['timestamp']?.toString(),
    );
  }
  final String app;
  final String listener;
  final ListenerHealth health;
  final String? timestamp;
}
