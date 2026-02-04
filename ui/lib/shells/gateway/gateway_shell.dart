import 'package:flutter/material.dart';
import 'gateway_controller.dart';
import 'widgets/device_selector.dart';
import '../../theme/piccolo_theme.dart';

/// The gateway shell is displayed when accessing piccolo.local.
///
/// It shows a device selector for multi-device LANs, allowing users
/// to choose which Piccolo device to access. For single-device LANs,
/// it auto-redirects to the device's specific hostname.
class GatewayShell extends StatefulWidget {
  const GatewayShell({super.key});

  @override
  State<GatewayShell> createState() => _GatewayShellState();
}

class _GatewayShellState extends State<GatewayShell> {
  late final GatewayController _controller;

  @override
  void initState() {
    super.initState();
    _controller = GatewayController();
    _controller.initialize();
  }

  @override
  void dispose() {
    _controller.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Piccolo',
      debugShowCheckedModeBanner: false,
      theme: PiccoloTheme.lightTheme,
      home: Scaffold(
        body: Container(
          decoration: const BoxDecoration(
            gradient: LinearGradient(
              begin: Alignment.topLeft,
              end: Alignment.bottomRight,
              colors: [
                Color(0xFF1a1a2e),
                Color(0xFF16213e),
                Color(0xFF0f3460),
              ],
            ),
          ),
          child: SafeArea(
            child: Center(
              child: ConstrainedBox(
                constraints: const BoxConstraints(maxWidth: 480),
                child: Padding(
                  padding: const EdgeInsets.all(24),
                  child: ListenableBuilder(
                    listenable: _controller,
                    builder: (context, _) {
                      if (_controller.isLoading || _controller.redirecting) {
                        return _buildLoadingView();
                      }
                      if (_controller.error != null) {
                        return _buildErrorView();
                      }
                      return _buildDeviceSelector();
                    },
                  ),
                ),
              ),
            ),
          ),
        ),
      ),
    );
  }

  Widget _buildLoadingView() {
    return Column(
      mainAxisAlignment: MainAxisAlignment.center,
      children: [
        _buildLogo(),
        const SizedBox(height: 32),
        const CircularProgressIndicator(color: Colors.white),
        const SizedBox(height: 16),
        Text(
          _controller.redirecting
              ? 'Redirecting to your device...'
              : 'Discovering devices...',
          style: const TextStyle(color: Colors.white70, fontSize: 14),
        ),
      ],
    );
  }

  Widget _buildErrorView() {
    return Column(
      mainAxisAlignment: MainAxisAlignment.center,
      children: [
        _buildLogo(),
        const SizedBox(height: 32),
        Icon(
          Icons.error_outline,
          color: Colors.red.shade300,
          size: 48,
        ),
        const SizedBox(height: 16),
        Text(
          _controller.error!,
          style: const TextStyle(color: Colors.white70, fontSize: 14),
          textAlign: TextAlign.center,
        ),
        const SizedBox(height: 24),
        ElevatedButton.icon(
          onPressed: _controller.refresh,
          icon: const Icon(Icons.refresh),
          label: const Text('Retry'),
          style: ElevatedButton.styleFrom(
            backgroundColor: Colors.white.withValues(alpha: 0.1),
            foregroundColor: Colors.white,
          ),
        ),
      ],
    );
  }

  Widget _buildDeviceSelector() {
    return Column(
      mainAxisAlignment: MainAxisAlignment.center,
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        _buildLogo(),
        const SizedBox(height: 24),
        const Text(
          'Select a device to continue:',
          style: TextStyle(
            color: Colors.white70,
            fontSize: 16,
          ),
          textAlign: TextAlign.center,
        ),
        const SizedBox(height: 24),
        Flexible(
          child: DeviceSelector(
            onlinePeers: _controller.onlinePeers,
            offlinePeers: _controller.offlinePeers,
            self: _controller.self,
            onDeviceSelected: _controller.navigateToDevice,
            onDeviceSelectedHttps: _controller.navigateToDeviceHttps,
          ),
        ),
        const SizedBox(height: 24),
        TextButton.icon(
          onPressed: _controller.refresh,
          icon: const Icon(Icons.refresh, size: 18),
          label: const Text('Refresh'),
          style: TextButton.styleFrom(
            foregroundColor: Colors.white70,
          ),
        ),
      ],
    );
  }

  Widget _buildLogo() {
    return Column(
      children: [
        // Piccolo logo/icon
        Container(
          width: 64,
          height: 64,
          decoration: BoxDecoration(
            color: Colors.white.withValues(alpha: 0.1),
            borderRadius: BorderRadius.circular(16),
          ),
          child: const Icon(
            Icons.dns,
            color: Colors.white,
            size: 32,
          ),
        ),
        const SizedBox(height: 16),
        const Text(
          'Piccolo',
          style: TextStyle(
            color: Colors.white,
            fontSize: 28,
            fontWeight: FontWeight.bold,
            letterSpacing: 1.2,
          ),
        ),
      ],
    );
  }
}
