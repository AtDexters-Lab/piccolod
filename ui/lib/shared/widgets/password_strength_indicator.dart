import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class PasswordStrengthIndicator extends StatelessWidget {

  const PasswordStrengthIndicator({required this.password, super.key});
  final String password;

  int _calculateStrength(String pass) {
    var score = 0;
    if (pass.isEmpty) return 0;
    if (pass.length >= 8) score++;
    if (pass.length >= 12) score++;
    if (pass.contains(RegExp('[A-Z]'))) score++;
    if (pass.contains(RegExp('[0-9]'))) score++;
    if (pass.contains(RegExp(r'[!@#$%^&*(),.?":{}|<>]'))) score++;
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
    if (password.isEmpty) return const SizedBox.shrink();
    final strength = _calculateStrength(password);

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
                          ? _getColor(strength)
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
              color: _getColor(strength),
              fontWeight: FontWeight.w600,
              fontSize: 12,
            ),
          ),
        ],
      ),
    );
  }
}
