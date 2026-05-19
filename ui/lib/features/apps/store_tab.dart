import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/config/core_config.dart';
import 'package:piccolo_os/core/models/app_models.dart';
import 'package:piccolo_os/core/services/app_service.dart';
import 'package:piccolo_os/features/apps/app_detail_view.dart';
import 'package:piccolo_os/features/apps/install_wizard.dart';
import 'package:piccolo_os/shared/widgets/app_icon.dart';
import 'package:piccolo_os/shells/desktop/desktop_controller.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class StoreTab extends StatefulWidget {

  const StoreTab({
    required this.appService, required this.searchQuery, required this.desktopController, required this.selectedCategory, super.key,
  });
  final AppService appService;
  final String searchQuery;
  final DesktopController desktopController;

  // Category filter (managed by parent)
  final String selectedCategory;

  @override
  State<StoreTab> createState() => _StoreTabState();
}

class _StoreTabState extends State<StoreTab> {
  List<CatalogItem> _items = [];
  bool _isLoading = true;
  String? _error;
  int _currentPage = 1;
  int _totalPages = 1;

  @override
  void initState() {
    super.initState();
    unawaited(_loadCatalog());
  }

  @override
  void didUpdateWidget(StoreTab oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (widget.searchQuery != oldWidget.searchQuery ||
        widget.selectedCategory != oldWidget.selectedCategory) {
      unawaited(_loadCatalog(reset: true));
    }
  }

  Future<void> _loadCatalog({bool reset = false, int? targetPage}) async {
    final pageToFetch = reset ? 1 : (targetPage ?? _currentPage);

    if (reset) {
      setState(() {
        _items = [];
        _currentPage = 1;
        _isLoading = true;
        _error = null;
      });
    } else {
      setState(() {
        _isLoading = true;
        _error = null;
      });
    }

    try {
      final response = await widget.appService.getCatalog(
        page: pageToFetch,
        query: widget.searchQuery,
        category: widget.selectedCategory == 'All'
            ? null
            : widget.selectedCategory,
      );
      if (!mounted) return;
      setState(() {
        if (reset) {
          _items = response.apps;
        } else {
          // Avoid duplicates if any
          final existingIds = _items.map((e) => e.name).toSet();
          for (final app in response.apps) {
            if (!existingIds.contains(app.name)) {
              _items.add(app);
            }
          }
        }
        _currentPage = response.page;
        _totalPages = response.totalPages;
        _isLoading = false;
      });
    } on Object catch (e) {
      if (!mounted) return;
      setState(() {
        _error = e.toString();
        _isLoading = false;
      });
    }
  }

  void _loadMore() {
    if (_currentPage < _totalPages && !_isLoading) {
      unawaited(_loadCatalog(targetPage: _currentPage + 1));
    }
  }

  Future<void> _installFromTemplate(CatalogItem item) async {
    var yaml = item.template;

    if (yaml == null || yaml.isEmpty) {
      try {
        yaml = await widget.appService.getCatalogTemplate(item.name);
      } on Object catch (e) {
        if (mounted) {
          ScaffoldMessenger.of(context).showSnackBar(
            SnackBar(content: Text('Failed to load template: $e')),
          );
        }
        return;
      }
    }

    if (yaml == null || !mounted) return;

    final proxyIconUrl = (item.icon ?? '').isNotEmpty
        ? CoreConfig.catalogIconUrl(item.name)
        : null;
    final originalIconUrl = item.icon;

    unawaited(showDialog<void>(
      context: context,
      barrierDismissible: false,
      builder: (context) => InstallWizard(
        appService: widget.appService,
        initialYaml: yaml,
        catalogSource: item.name,
        displayName: item.name,
        onSuccess: (appName) {
          Navigator.of(context).pop();
          _openAppDetail(appName, iconUrl: proxyIconUrl, originalIconUrl: originalIconUrl);
        },
      ),
    ));
  }

  void _openAppDetail(String appName, {String? iconUrl, String? originalIconUrl}) {
    widget.desktopController.notifyAppsChanged();
    widget.desktopController.openApp(
      'app-detail-$appName',
      appName,
      PiccoloIcons.settingsApp,
      AppDetailView(
        appId: appName,
        appService: widget.appService,
        desktopController: widget.desktopController,
        iconUrl: iconUrl,
        originalIconUrl: originalIconUrl,
      ),
      iconUrl: iconUrl,
      originalIconUrl: originalIconUrl,
    );
  }

  @override
  Widget build(BuildContext context) {
    return _buildContent();
  }

  Widget _buildContent() {
    if (_isLoading && _items.isEmpty) {
      return const Center(child: CircularProgressIndicator());
    }

    if (_error != null && _items.isEmpty) {
      return Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            const Icon(
              PiccoloIcons.error,
              color: PiccoloTheme.critical,
              size: 48,
            ),
            const SizedBox(height: Spacing.base),
            Text(_error!, style: PiccoloTheme.textTheme.bodyMedium),
            const SizedBox(height: Spacing.base),
            OutlinedButton(
              onPressed: () => _loadCatalog(reset: true),
              child: const Text('Retry'),
            ),
          ],
        ),
      );
    }

    if (_items.isEmpty) {
      return Center(
        child: Text(
          widget.searchQuery.isEmpty
              ? 'No apps found in this category.'
              : 'No matching apps found.',
          style: PiccoloTheme.textTheme.bodyMedium?.copyWith(
            color: PiccoloTheme.inkMuted,
          ),
        ),
      );
    }

    return ListView(
      padding: const EdgeInsets.all(Spacing.lg),
      children: [
        GridView.builder(
          shrinkWrap: true,
          physics: const NeverScrollableScrollPhysics(),
          gridDelegate: const SliverGridDelegateWithMaxCrossAxisExtent(
            maxCrossAxisExtent: 350,
            mainAxisSpacing: Spacing.base,
            crossAxisSpacing: Spacing.base,
            childAspectRatio: 0.85,
          ),
          itemCount: _items.length,
          itemBuilder: (context, index) {
            final item = _items[index];
            return _CatalogCard(
              item: item,
              onInstall: () => _installFromTemplate(item),
            );
          },
        ),
        if (_currentPage < _totalPages)
          Padding(
            padding: const EdgeInsets.symmetric(vertical: Spacing.xl),
            child: Center(
              child: OutlinedButton(
                onPressed: _isLoading ? null : _loadMore,
                child: _isLoading
                    ? const SizedBox(
                        width: 20,
                        height: 20,
                        child: CircularProgressIndicator(strokeWidth: 2),
                      )
                    : const Text('Load More'),
              ),
            ),
          ),
      ],
    );
  }
}

class _CatalogCard extends StatelessWidget {

  const _CatalogCard({required this.item, required this.onInstall});
  final CatalogItem item;
  final VoidCallback onInstall;

  @override
  Widget build(BuildContext context) {
    return Card(
      elevation: 0,
      color: PiccoloTheme.porcelain,
      shape: RoundedRectangleBorder(
        borderRadius: BorderRadius.circular(Radii.md),
        side: const BorderSide(color: PiccoloTheme.hairline),
      ),
      child: Padding(
        padding: const EdgeInsets.all(Spacing.base),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            // Header
            Row(
              children: [
                AppIcon(
                  proxyUrl: (item.icon ?? '').isNotEmpty
                      ? CoreConfig.catalogIconUrl(item.name)
                      : null,
                  originalIconUrl: item.icon,
                  borderRadius: 12,
                  fallbackIcon: PiccoloIcons.apps,
                ),
                const SizedBox(width: Spacing.md),
                Expanded(
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      Text(
                        item.name,
                        style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
                          fontWeight: FontWeight.bold,
                        ),
                        maxLines: 1,
                        overflow: TextOverflow.ellipsis,
                      ),
                      const SizedBox(height: Spacing.xs),
                      Text(
                        '${item.category} • v${item.version}',
                        style: PiccoloTheme.textTheme.labelSmall?.copyWith(
                          color: PiccoloTheme.inkMuted,
                        ),
                      ),
                    ],
                  ),
                ),
              ],
            ),
            const SizedBox(height: Spacing.md),
            // Description
            Expanded(
              child: Text(
                item.description,
                style: PiccoloTheme.textTheme.bodyMedium?.copyWith(
                  color: PiccoloTheme.inkMuted,
                ),
                maxLines: 3,
                overflow: TextOverflow.ellipsis,
              ),
            ),
            const SizedBox(height: Spacing.sm),
            if (item.tags.isNotEmpty)
              Wrap(
                spacing: Spacing.xs,
                runSpacing: Spacing.xs,
                children: item.tags
                    .take(3)
                    .map(
                      (tag) => Container(
                        padding: const EdgeInsets.symmetric(
                          horizontal: 6,
                          vertical: 2,
                        ),
                        decoration: BoxDecoration(
                          color: PiccoloTheme.overlay,
                          borderRadius: BorderRadius.circular(Radii.xxs),
                        ),
                        child: Text(
                          tag,
                          style: PiccoloTheme.textTheme.labelSmall?.copyWith(
                            fontSize: 10,
                          ),
                        ),
                      ),
                    )
                    .toList(),
              ),
            const SizedBox(height: Spacing.md),
            // Footer Action
            SizedBox(
              width: double.infinity,
              child: FilledButton(
                onPressed: onInstall,
                child: const Text('Install'),
              ),
            ),
          ],
        ),
      ),
    );
  }
}
