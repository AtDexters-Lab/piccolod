import 'package:flutter/material.dart';
import '../../theme/piccolo_theme.dart';

class PasswordStrengthIndicator extends StatelessWidget {
  final String password;

  const PasswordStrengthIndicator({super.key, required this.password});

  int _calculateStrength(String pass) {
    int score = 0;
    if (pass.isEmpty) return 0;
    if (pass.length >= 8) score++;
    if (pass.length >= 12) score++;
    if (pass.contains(RegExp(r'[A-Z]'))) score++;
    if (pass.contains(RegExp(r'[0-9]'))) score++;
    if (pass.contains(RegExp(r'[!@#$%^&*(),.?":{}|<>]'))) score++;
    return score;
  }

  Color _getColor(int score) {
    if (score <= 1) return Colors.red;
    if (score <= 2) return Colors.orange;
    if (score <= 3) return Colors.amber;
    if (score <= 4) return Colors.lightGreen;
    return Colors.green;
  }

  String _getLabel(int score) {
    if (score <= 1) return "Very weak";
    if (score <= 2) return "Weak";
    if (score <= 3) return "Moderate";
    return "Strong";
  }

  @override
  Widget build(BuildContext context) {
    if (password.isEmpty) return const SizedBox.shrink();
    final strength = _calculateStrength(password);

    return Padding(
      padding: const EdgeInsets.only(top: 8.0, bottom: 4.0),
      child: Row(
        children: [
          Expanded(
            child: Row(
              children: List.generate(5, (index) {
                return Expanded(
                  child: Container(
                    margin: const EdgeInsets.only(right: 4),
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
          const SizedBox(width: 12),
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
