import 'package:flutter/material.dart';

/// Renders the "P" mark from piccolo-p.svg / piccolo-p-white.svg.
/// Path data extracted from @ui-next/static/piccolo-p.svg
class PiccoloLogo extends StatelessWidget {
  final double size;
  final Color color;

  const PiccoloLogo({super.key, this.size = 24, required this.color});

  @override
  Widget build(BuildContext context) {
    return SizedBox(
      width: size,
      height: size,
      child: CustomPaint(painter: _PiccoloLogoPainter(color)),
    );
  }
}

class _PiccoloLogoPainter extends CustomPainter {
  final Color color;

  _PiccoloLogoPainter(this.color);

  @override
  void paint(Canvas canvas, Size size) {
    // The original viewbox is roughly 0 0 399 399.
    // We scale it to fit 'size'.
    final double scale = size.width / 399.0;

    canvas.scale(scale, scale);

    final paint = Paint()
      ..color = color
      ..style = PaintingStyle.fill;

    // Path data from piccolo-p.svg (transform translated 95 -45 removed/adjusted or kept if part of path)
    // The SVG group has transform="translate(95 -45)".
    // We need to apply this translation or adjust the path.
    // Since I cannot easily re-calculate the path points, I will apply the translation to the canvas.

    canvas.translate(95, -45);

    final path = Path();
    path.moveTo(18.0706, 399);
    path.cubicTo(12.8328, 399, 8.51153, 397.297, 5.10692, 393.89);
    path.cubicTo(1.70231, 390.483, 0, 386.159, 0, 380.917);
    path.lineTo(0, 198.91);
    path.cubicTo(0.261894, 178.469, 5.10692, 159.993, 14.5351, 143.483);
    path.cubicTo(24.2251, 126.972, 37.1888, 114, 53.4262, 104.566);
    path.cubicTo(69.6636, 95.1311, 87.9961, 90.4138, 108.424, 90.4138);
    path.cubicTo(129.113, 90.4138, 147.577, 95.2621, 163.814, 104.959);
    path.cubicTo(180.313, 114.393, 193.277, 127.366, 202.705, 143.876);
    path.cubicTo(212.395, 160.124, 217.24, 178.731, 217.24, 199.697);
    path.cubicTo(217.24, 220.4, 212.657, 239.007, 203.491, 255.517);
    path.cubicTo(194.587, 272.028, 182.409, 285, 166.957, 294.435);
    path.cubicTo(151.505, 303.869, 134.089, 308.586, 114.709, 308.586);
    path.cubicTo(98.2099, 308.586, 83.282, 305.179, 69.9255, 298.366);
    path.cubicTo(65.3253, 296.019, 60.9736, 293.392, 56.8705, 290.486);
    path.cubicTo(49.1441, 285.013, 36.1412, 290.03, 36.1412, 299.498);
    path.lineTo(36.1412, 380.917);
    path.cubicTo(36.1412, 386.159, 34.4389, 390.483, 31.0343, 393.89);
    path.cubicTo(27.8916, 397.297, 23.5704, 399, 18.0706, 399);
    path.close();

    path.moveTo(108.424, 276.352);
    path.cubicTo(122.304, 276.352, 134.744, 273.076, 145.743, 266.524);
    path.cubicTo(157.005, 259.71, 165.778, 250.538, 172.064, 239.007);
    path.cubicTo(178.611, 227.214, 181.885, 214.11, 181.885, 199.697);
    path.cubicTo(181.885, 185.021, 178.611, 171.917, 172.064, 160.386);
    path.cubicTo(165.778, 148.593, 157.005, 139.421, 145.743, 132.869);
    path.cubicTo(134.744, 126.055, 122.304, 122.648, 108.424, 122.648);
    path.cubicTo(94.5434, 122.648, 81.9725, 126.055, 70.7111, 132.869);
    path.cubicTo(59.7116, 139.683, 51.0692, 148.855, 44.7837, 160.386);
    path.cubicTo(38.4983, 171.917, 35.3556, 185.021, 35.3556, 199.697);
    path.cubicTo(35.3556, 214.11, 38.4983, 227.214, 44.7837, 239.007);
    path.cubicTo(51.0692, 250.538, 59.7116, 259.71, 70.7111, 266.524);
    path.cubicTo(81.9725, 273.076, 94.5434, 276.352, 108.424, 276.352);
    path.close();

    canvas.drawPath(path, paint);
  }

  @override
  bool shouldRepaint(covariant CustomPainter oldDelegate) => false;
}
