import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Unicode-aware password policy checks matching the backend
/// (`internal/auth/password_policy.go`).
class PasswordPolicy {
  static const minLength = 8;
  static final _hasLetter = RegExp(r'\p{L}', unicode: true);
  static final _hasNumber = RegExp(r'\p{N}', unicode: true);

  static bool meetsLength(String p) => p.runes.length >= minLength;
  static bool hasLetter(String p) => _hasLetter.hasMatch(p);
  static bool hasNumber(String p) => _hasNumber.hasMatch(p);
  static bool isValid(String p) => meetsLength(p) && hasLetter(p) && hasNumber(p);

  /// Returns the first unmet requirement as an error string, or null if valid.
  static String? validate(String password) {
    if (!meetsLength(password)) return 'Password must be at least 8 characters';
    if (!hasLetter(password)) return 'Password must include at least one letter';
    if (!hasNumber(password)) return 'Password must include at least one number';
    return null;
  }
}

class PasswordStrengthIndicator extends StatelessWidget {

  const PasswordStrengthIndicator({required this.password, super.key});
  final String password;

  static final _hasUppercase = RegExp('[A-Z]');
  static final _hasDigit = RegExp('[0-9]');
  static final _hasSpecial = RegExp(r'[!@#$%^&*(),.?":{}|<>]');

  int _calculateStrength(String pass) {
    var score = 0;
    if (pass.isEmpty) return 0;
    final runes = pass.runes.length;
    if (runes >= 8) score++;
    if (runes >= 12) score++;
    if (_hasUppercase.hasMatch(pass)) score++;
    if (_hasDigit.hasMatch(pass)) score++;
    if (_hasSpecial.hasMatch(pass)) score++;
    return score;
  }

  Color _getColor(int score) {
    if (score <= 1) return PiccoloTheme.critical;
    if (score <= 2) return PiccoloTheme.warning;
    if (score <= 3) return PiccoloTheme.warning;
    return PiccoloTheme.success;
  }

  String _getLabel(int score) {
    if (score <= 1) return 'Very weak';
    if (score <= 2) return 'Weak';
    if (score <= 3) return 'Moderate';
    return 'Strong';
  }

  @override
  Widget build(BuildContext context) {
    final hasInput = password.isNotEmpty;
    final meetsLength = PasswordPolicy.meetsLength(password);
    final hasLetter = PasswordPolicy.hasLetter(password);
    final hasNumber = PasswordPolicy.hasNumber(password);

    return Column(
      mainAxisSize: MainAxisSize.min,
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        // Strength bars — only when typing
        if (hasInput) ...[
          () {
            final strength = _calculateStrength(password);
            final color = _getColor(strength);
            return Padding(
              padding: const EdgeInsets.only(top: Spacing.sm, bottom: Spacing.xs),
              child: Row(
                children: [
                  Expanded(
                    child: Row(
                      children: List.generate(5, (index) {
                        return Expanded(
                          child: Container(
                            margin: const EdgeInsets.only(right: Spacing.xs),
                            height: 4,
                            decoration: BoxDecoration(
                              color: index < strength
                                  ? color
                                  : PiccoloTheme.mist,
                              borderRadius: BorderRadius.circular(2),
                            ),
                          ),
                        );
                      }),
                    ),
                  ),
                  const SizedBox(width: Spacing.md),
                  Text(
                    _getLabel(strength),
                    style: TextStyle(
                      color: color,
                      fontWeight: FontWeight.w600,
                      fontSize: 12,
                    ),
                  ),
                ],
              ),
            );
          }(),
        ],
        // Requirement hints — always visible
        const SizedBox(height: Spacing.xs),
        _RequirementRow(met: hasInput && meetsLength, label: 'At least 8 characters'),
        _RequirementRow(met: hasInput && hasLetter, label: 'Contains a letter'),
        _RequirementRow(met: hasInput && hasNumber, label: 'Contains a number'),
      ],
    );
  }
}

class _RequirementRow extends StatelessWidget {
  const _RequirementRow({required this.met, required this.label});
  final bool met;
  final String label;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.only(bottom: 2),
      child: Row(
        children: [
          Icon(
            met ? PiccoloIcons.check : PiccoloIcons.minimize,
            size: 14,
            color: met ? PiccoloTheme.success : PiccoloTheme.inkMuted,
          ),
          const SizedBox(width: 6),
          Text(
            label,
            style: TextStyle(
              fontSize: 12,
              color: met ? PiccoloTheme.success : PiccoloTheme.inkMuted,
            ),
          ),
        ],
      ),
    );
  }
}
