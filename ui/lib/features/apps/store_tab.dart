import 'package:flutter/material.dart';
import '../../core/models/app_models.dart';
import '../../core/services/app_service.dart';
import '../../theme/piccolo_theme.dart';
import '../../shells/desktop/desktop_controller.dart';
import 'custom_install_wizard.dart';
import 'app_detail_view.dart';

class StoreTab extends StatefulWidget {
  final AppService appService;
  final String searchQuery;
  final VoidCallback onInstallCustom;
  final DesktopController desktopController;

  const StoreTab({
    super.key,
    required this.appService,
    required this.searchQuery,
    required this.onInstallCustom,
    required this.desktopController,
  });

  @override
  State<StoreTab> createState() => _StoreTabState();
}

class _StoreTabState extends State<StoreTab> {
  List<CatalogItem> _allItems = [];
  bool _isLoading = true;
  String? _error;

  @override
  void initState() {
    super.initState();
    _loadCatalog();
  }

  Future<void> _loadCatalog() async {
    setState(() {
      _isLoading = true;
      _error = null;
    });

    try {
      final items = await widget.appService.getCatalog();
      if (!mounted) return;
      setState(() {
        _allItems = items;
        _isLoading = false;
      });
    } catch (e) {
      if (!mounted) return;
      setState(() {
        _error = e.toString();
        _isLoading = false;
      });
    }
  }

  void _installFromTemplate(CatalogItem item) async {
    String? yaml = item.template;
    
    if (yaml == null || yaml.isEmpty) {
        try {
            yaml = await widget.appService.getCatalogTemplate(item.name);
        } catch (e) {
            if (mounted) {
                ScaffoldMessenger.of(context).showSnackBar(
                    SnackBar(content: Text("Failed to load template: $e")),
                );
            }
            return;
        }
    }

    if (yaml != null && mounted) {
      showDialog(
        context: context,
        barrierDismissible: false,
        builder: (context) => CustomInstallWizard(
          appService: widget.appService,
          initialYaml: yaml,
          onSuccess: (appName) {
             Navigator.of(context).pop(); // Close Wizard
             // Open App Detail Window
             widget.desktopController.openApp(
               "app-detail-$appName",
               appName,
               Icons.settings_applications,
               AppDetailView(
                 appId: appName,
                 appService: widget.appService,
                 desktopController: widget.desktopController,
               ),
             );
          },
        ),
      );
    }
  }

  @override
  Widget build(BuildContext context) {
    if (_isLoading) {
      return const Center(child: CircularProgressIndicator());
    }

    if (_error != null) {
      return Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            const Icon(Icons.error_outline, color: PiccoloTheme.critical, size: 48),
            const SizedBox(height: 16),
            Text(_error!, style: PiccoloTheme.textTheme.bodyMedium),
            const SizedBox(height: 16),
            OutlinedButton(
              onPressed: _loadCatalog,
              child: const Text("Retry"),
            ),
          ],
        ),
      );
    }

    final filteredItems = _allItems.where((item) {
      final q = widget.searchQuery.toLowerCase();
      return item.name.toLowerCase().contains(q) ||
          item.description.toLowerCase().contains(q);
    }).toList();

    if (filteredItems.isEmpty) {
      return Center(
        child: Text(
          widget.searchQuery.isEmpty ? "No apps in catalog." : "No matching apps found.",
          style: PiccoloTheme.textTheme.bodyMedium?.copyWith(
            color: PiccoloTheme.inkMuted,
          ),
        ),
      );
    }

    return GridView.builder(
      padding: const EdgeInsets.all(24),
      gridDelegate: const SliverGridDelegateWithMaxCrossAxisExtent(
        maxCrossAxisExtent: 300,
        mainAxisSpacing: 16,
        crossAxisSpacing: 16,
        childAspectRatio: 0.8, // Taller cards for description
      ),
      itemCount: filteredItems.length,
      itemBuilder: (context, index) {
        final item = filteredItems[index];
        return _CatalogCard(
          item: item,
          onInstall: () => _installFromTemplate(item),
        );
      },
    );
  }
}

class _CatalogCard extends StatelessWidget {
  final CatalogItem item;
  final VoidCallback onInstall;

  const _CatalogCard({required this.item, required this.onInstall});

  @override
  Widget build(BuildContext context) {
    return Card(
      elevation: 0,
      color: PiccoloTheme.porcelain,
      shape: RoundedRectangleBorder(
        borderRadius: BorderRadius.circular(12),
        side: const BorderSide(color: Colors.black12),
      ),
      child: Padding(
        padding: const EdgeInsets.all(16.0),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            // Header
            Row(
              children: [
                Container(
                  width: 48,
                  height: 48,
                  decoration: BoxDecoration(
                    color: PiccoloTheme.mist,
                    borderRadius: BorderRadius.circular(12),
                    image: item.image.isNotEmpty ? DecorationImage(
                        image: NetworkImage(item.image), // Assumption: URL accessible
                        fit: BoxFit.cover,
                    ) : null,
                  ),
                  child: item.image.isEmpty 
                    ? const Icon(Icons.apps, color: PiccoloTheme.inkMuted)
                    : null,
                ),
                const SizedBox(width: 12),
                Expanded(
                  child: Text(
                    item.name,
                    style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
                      fontWeight: FontWeight.bold,
                    ),
                    maxLines: 2,
                    overflow: TextOverflow.ellipsis,
                  ),
                ),
              ],
            ),
            const SizedBox(height: 12),
            // Description
            Expanded(
              child: Text(
                item.description,
                style: PiccoloTheme.textTheme.bodyMedium?.copyWith(
                  color: PiccoloTheme.inkMuted,
                ),
                maxLines: 4,
                overflow: TextOverflow.ellipsis,
              ),
            ),
            const SizedBox(height: 12),
            // Footer Action
            SizedBox(
              width: double.infinity,
              child: FilledButton(
                onPressed: onInstall,
                style: FilledButton.styleFrom(
                  backgroundColor: PiccoloTheme.cobalt600,
                  foregroundColor: Colors.white,
                ),
                child: const Text("Install"),
              ),
            ),
          ],
        ),
      ),
    );
  }
}
