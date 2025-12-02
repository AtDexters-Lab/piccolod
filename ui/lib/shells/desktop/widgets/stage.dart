import 'package:flutter/material.dart';
import '../../../theme/piccolo_theme.dart';

class Stage extends StatefulWidget {
  const Stage({super.key});

  @override
  State<Stage> createState() => _StageState();
}

class _StageState extends State<Stage> with SingleTickerProviderStateMixin {
  late AnimationController _controller;
  late Animation<Alignment> _topAlignmentAnimation;
  late Animation<Alignment> _bottomAlignmentAnimation;

  @override
  void initState() {
    super.initState();
    _controller = AnimationController(
      vsync: this,
      duration: const Duration(seconds: 10), // Faster for visibility
    )..repeat(reverse: true);

    _topAlignmentAnimation = AlignmentTween(
      begin: const Alignment(-0.8, -0.6), // Start far top-left
      end: const Alignment(0.8, 0.2), // End far right-middle
    ).animate(CurvedAnimation(parent: _controller, curve: Curves.easeInOut));

    _bottomAlignmentAnimation = AlignmentTween(
      begin: Alignment.bottomRight,
      end: Alignment.centerLeft, // Move significantly
    ).animate(CurvedAnimation(parent: _controller, curve: Curves.easeInOut));
  }

  @override
  void dispose() {
    _controller.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return AnimatedBuilder(
      animation: _controller,
      builder: (context, child) {
        return Container(
          width: double.infinity,
          height: double.infinity,
          decoration: BoxDecoration(
            // Animated Hero Gradient
            gradient: LinearGradient(
              begin: _topAlignmentAnimation.value,
              end: _bottomAlignmentAnimation.value,
              colors: const [
                Color(0xFFF6F8FC),
                Color(0xFFE9EEF6),
                Color(0xFFE4EAF3),
              ],
              stops: const [0.0, 0.5, 1.0],
            ),
          ),
          child: child,
        );
      },
      child: Center(
        child: Text(
          "The Stage",
          style: PiccoloTheme.textTheme.displayLarge?.copyWith(
            color: PiccoloTheme.ink.withValues(alpha: 0.05), // Very subtle
          ),
        ),
      ),
    );
  }
}
