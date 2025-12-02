import 'package:flutter/material.dart';
import '../../../theme/piccolo_theme.dart';
import '../../../shared/piccolo_wordmark.dart';

class TopBar extends StatelessWidget {
  const TopBar({super.key});

  @override
  Widget build(BuildContext context) {
    return Container(
      height: 48, // Fixed height for Layer A
      padding: const EdgeInsets.symmetric(horizontal: 24),
      color: Colors.transparent, // "Frosted" effect would be applied here
      child: Row(
        children: [
          // -- Branding --
          const PiccoloWordmark(
            height: 18, // Slightly smaller to fit nicely in 48px bar
            color: PiccoloTheme.ink,
          ),
          
          const SizedBox(width: 32),
          
          // -- Global Search --
          IconButton(
            icon: const Icon(Icons.search, color: PiccoloTheme.ink, size: 20),
            onPressed: () {},
            splashRadius: 20,
            tooltip: "Search",
          ),

          const Spacer(),
          
          // -- Trailing: System Health / User --
          _SystemStatusChip(
            label: "Healthy",
            color: PiccoloTheme.success,
          ),
          const SizedBox(width: 16),
          const CircleAvatar(
            radius: 14,
            backgroundColor: PiccoloTheme.cobalt600,
            child: Text("A", style: TextStyle(color: Colors.white, fontSize: 12)),
          ),
        ],
      ),
    );
  }
}

class _SystemStatusChip extends StatelessWidget {
  final String label;
  final Color color;

  const _SystemStatusChip({required this.label, required this.color});

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 6),
      decoration: BoxDecoration(
        color: color.withValues(alpha: 0.1),
        borderRadius: BorderRadius.circular(20),
        border: Border.all(color: color.withValues(alpha: 0.2)),
      ),
      child: Row(
        children: [
          Container(
            width: 6,
            height: 6,
            decoration: BoxDecoration(color: color, shape: BoxShape.circle),
          ),
          const SizedBox(width: 8),
          Text(
            label,
            style: PiccoloTheme.textTheme.labelSmall?.copyWith(
              color: PiccoloTheme.ink,
              fontWeight: FontWeight.w500,
            ),
          ),
        ],
      ),
    );
  }
}
