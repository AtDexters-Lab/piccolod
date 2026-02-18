import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/shells/gateway/gateway_controller.dart';
import 'package:piccolo_os/shells/gateway/widgets/device_selector.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

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
    unawaited(_controller.initialize());
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
                PiccoloTheme.ink,
                Color(0xFF1A2030),
                Color(0xFF0F1828),
              ],
            ),
          ),
          child: SafeArea(
            child: Center(
              child: ConstrainedBox(
                constraints: const BoxConstraints(maxWidth: 480),
                child: Padding(
                  padding: const EdgeInsets.all(Spacing.lg),
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
        const SizedBox(height: Spacing.xl),
        const CircularProgressIndicator(color: Colors.white),
        const SizedBox(height: Spacing.base),
        Text(
          _controller.redirecting
              ? 'Redirecting to your device...'
              : 'Discovering devices...',
          style: TextStyle(
            color: Colors.white.withValues(alpha: 0.7),
            fontSize: 14,
          ),
        ),
      ],
    );
  }

  Widget _buildErrorView() {
    return Column(
      mainAxisAlignment: MainAxisAlignment.center,
      children: [
        _buildLogo(),
        const SizedBox(height: Spacing.xl),
        const Icon(
          PiccoloIcons.error,
          color: PiccoloTheme.critical,
          size: 48,
        ),
        const SizedBox(height: Spacing.base),
        Text(
          _controller.error!,
          style: TextStyle(
            color: Colors.white.withValues(alpha: 0.7),
            fontSize: 14,
          ),
          textAlign: TextAlign.center,
        ),
        const SizedBox(height: Spacing.lg),
        FilledButton.icon(
          onPressed: _controller.refresh,
          icon: const Icon(PiccoloIcons.refresh),
          label: const Text('Retry'),
          style: FilledButton.styleFrom(
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
        const SizedBox(height: Spacing.lg),
        Text(
          'Select a device to continue:',
          style: TextStyle(
            color: Colors.white.withValues(alpha: 0.7),
            fontSize: 16,
          ),
          textAlign: TextAlign.center,
        ),
        const SizedBox(height: Spacing.lg),
        Flexible(
          child: DeviceSelector(
            onlinePeers: _controller.onlinePeers,
            offlinePeers: _controller.offlinePeers,
            self: _controller.self,
            onDeviceSelected: _controller.navigateToDevice,
            onDeviceSelectedHttps: _controller.navigateToDeviceHttps,
          ),
        ),
        const SizedBox(height: Spacing.lg),
        TextButton.icon(
          onPressed: _controller.refresh,
          icon: const Icon(PiccoloIcons.refresh, size: 18),
          label: const Text('Refresh'),
          style: TextButton.styleFrom(
            foregroundColor: Colors.white.withValues(alpha: 0.7),
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
            borderRadius: BorderRadius.circular(Radii.md),
          ),
          child: const Icon(
            PiccoloIcons.hardDrives,
            color: Colors.white,
            size: 32,
          ),
        ),
        const SizedBox(height: Spacing.base),
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
