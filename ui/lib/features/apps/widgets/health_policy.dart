enum ListenerHealthAction {
  none,
  showWarningBadge,
  showInfoBanner,
  showRecoveryOverlay,
  showErrorOverlay,
}

class ListenerHealthPolicy {

  const ListenerHealthPolicy({
    required this.cardAction,
    required this.detailAction,
    this.offerLocalFallback = false,
    this.fallbackMessage,
  });
  final ListenerHealthAction cardAction;
  final ListenerHealthAction detailAction;
  final bool offerLocalFallback;
  final String? fallbackMessage;
}

const Map<String, ListenerHealthPolicy> healthPolicies = {
  'ok': ListenerHealthPolicy(
    cardAction: ListenerHealthAction.none,
    detailAction: ListenerHealthAction.none,
  ),
  'degraded': ListenerHealthPolicy(
    cardAction: ListenerHealthAction.showWarningBadge,
    detailAction: ListenerHealthAction.showInfoBanner,
  ),
  'recovering': ListenerHealthPolicy(
    cardAction: ListenerHealthAction.showWarningBadge,
    detailAction: ListenerHealthAction.showRecoveryOverlay,
    offerLocalFallback: true,
    fallbackMessage:
        'Remote access is being set up. You can access this app locally while we work on it.',
  ),
  'error': ListenerHealthPolicy(
    cardAction: ListenerHealthAction.showWarningBadge,
    detailAction: ListenerHealthAction.showErrorOverlay,
    offerLocalFallback: true,
    fallbackMessage: 'Remote access is temporarily unavailable.',
  ),
};

ListenerHealthPolicy policyForStatus(String? status) {
  if (status == null) return healthPolicies['ok']!;
  return healthPolicies[status] ?? healthPolicies['error']!;
}
