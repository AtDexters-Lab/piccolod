class CapabilityProviderStatus {
  const CapabilityProviderStatus({
    required this.appInstance,
    required this.enabled,
  });

  factory CapabilityProviderStatus.fromJson(Map<String, dynamic> json) {
    return CapabilityProviderStatus(
      appInstance: (json['app_instance'] ?? '').toString(),
      enabled: json['enabled'] == true,
    );
  }

  final String appInstance;
  final bool enabled;
}

enum CapabilityProviderSelectionOutcome {
  reconciled,
  repairPending;

  static CapabilityProviderSelectionOutcome fromHttpStatus(int statusCode) {
    return statusCode == 202 ? repairPending : reconciled;
  }
}

class CapabilityStatus {
  const CapabilityStatus({
    required this.capability,
    required this.defaultProvider,
    required this.providers,
    required this.providerChangeDisclosure,
  });

  factory CapabilityStatus.fromJson(Map<String, dynamic> json) {
    final rawProviders = json['providers'];
    return CapabilityStatus(
      capability: (json['capability'] ?? '').toString(),
      defaultProvider: (json['default'] ?? '').toString(),
      providers: rawProviders is List<dynamic>
          ? rawProviders
                .whereType<Map<dynamic, dynamic>>()
                .map(
                  (provider) => CapabilityProviderStatus.fromJson(
                    Map<String, dynamic>.from(provider),
                  ),
                )
                .toList()
          : const [],
      providerChangeDisclosure: (json['provider_change_disclosure'] ?? '')
          .toString(),
    );
  }

  final String capability;
  final String defaultProvider;
  final List<CapabilityProviderStatus> providers;
  final String providerChangeDisclosure;

  CapabilityProviderStatus? providerFor(String appInstance) {
    for (final provider in providers) {
      if (provider.appInstance == appInstance) return provider;
    }
    return null;
  }

  bool isDefault(String appInstance) => defaultProvider == appInstance;
}
