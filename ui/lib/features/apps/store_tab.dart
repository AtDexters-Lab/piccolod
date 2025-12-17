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
  List<CatalogItem> _items = [];
  bool _isLoading = true;
  String? _error;
  int _currentPage = 1;
  int _totalPages = 1;
  
  List<String> _categories = ['All'];
  String _selectedCategory = 'All';

  @override
  void initState() {
    super.initState();
    _loadCategories();
    _loadCatalog();
  }

  @override
  void didUpdateWidget(StoreTab oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (widget.searchQuery != oldWidget.searchQuery) {
      _loadCatalog(reset: true);
    }
  }

  Future<void> _loadCategories() async {
    try {
      final cats = await widget.appService.getCategories();
      if (!mounted) return;
      setState(() {
        _categories = ['All', ...cats];
      });
    } catch (e) {
      // Log error but don't block main UI
      debugPrint("Failed to load categories: $e");
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
        pageSize: 20,
        query: widget.searchQuery,
        category: _selectedCategory == 'All' ? null : _selectedCategory,
      );
      if (!mounted) return;
      setState(() {
        if (reset) {
          _items = response.apps;
        } else {
          // Avoid duplicates if any
          final existingIds = _items.map((e) => e.name).toSet();
          for (var app in response.apps) {
            if (!existingIds.contains(app.name)) {
              _items.add(app);
            }
          }
        }
        _currentPage = response.page;
        _totalPages = response.totalPages;
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

  void _onCategorySelected(String category) {
    if (_selectedCategory == category) return;
    setState(() {
      _selectedCategory = category;
    });
    _loadCatalog(reset: true);
  }

  void _loadMore() {
    if (_currentPage < _totalPages && !_isLoading) {
      _loadCatalog(targetPage: _currentPage + 1);
    }
  }

  // ... (keep _installFromTemplate) ...
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
          initialYaml: yaml!,
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
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        // Category Pills
        Container(
          height: 60,
          padding: const EdgeInsets.symmetric(vertical: 12),
          child: ListView.separated(
            padding: const EdgeInsets.symmetric(horizontal: 24),
            scrollDirection: Axis.horizontal,
            itemCount: _categories.length,
            separatorBuilder: (c, i) => const SizedBox(width: 8),
            itemBuilder: (context, index) {
              final cat = _categories[index];
              final isSelected = cat == _selectedCategory;
              return ChoiceChip(
                label: Text(cat),
                selected: isSelected,
                showCheckmark: false,
                onSelected: (_) => _onCategorySelected(cat),
                selectedColor: PiccoloTheme.cobalt600,
                labelStyle: TextStyle(
                  color: isSelected ? Colors.white : PiccoloTheme.ink,
                  fontWeight: isSelected ? FontWeight.bold : FontWeight.normal,
                ),
                backgroundColor: PiccoloTheme.porcelain,
                side: BorderSide.none,
                shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(20)),
              );
            },
          ),
        ),
        
        // Content
        Expanded(
          child: _buildContent(),
        ),
      ],
    );
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
            const Icon(Icons.error_outline, color: PiccoloTheme.critical, size: 48),
            const SizedBox(height: 16),
            Text(_error!, style: PiccoloTheme.textTheme.bodyMedium),
            const SizedBox(height: 16),
            OutlinedButton(
              onPressed: () => _loadCatalog(reset: true),
              child: const Text("Retry"),
            ),
          ],
        ),
      );
    }

    if (_items.isEmpty) {
      return Center(
        child: Text(
          widget.searchQuery.isEmpty 
             ? "No apps found in this category." 
             : "No matching apps found.",
          style: PiccoloTheme.textTheme.bodyMedium?.copyWith(
            color: PiccoloTheme.inkMuted,
          ),
        ),
      );
    }

    return ListView(
      padding: const EdgeInsets.all(24),
      children: [
        GridView.builder(
          shrinkWrap: true,
          physics: const NeverScrollableScrollPhysics(),
          gridDelegate: const SliverGridDelegateWithMaxCrossAxisExtent(
            maxCrossAxisExtent: 350,
            mainAxisSpacing: 16,
            crossAxisSpacing: 16,
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
            padding: const EdgeInsets.symmetric(vertical: 32.0),
            child: Center(
              child: OutlinedButton(
                onPressed: _isLoading ? null : _loadMore,
                child: _isLoading
                    ? const SizedBox(width: 20, height: 20, child: CircularProgressIndicator(strokeWidth: 2))
                    : const Text("Load More"),
              ),
            ),
          ),
      ],
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
                    image: (item.icon != null && item.icon!.isNotEmpty) ? DecorationImage(
                        image: NetworkImage(item.icon!), 
                        fit: BoxFit.cover,
                    ) : null,
                  ),
                  child: (item.icon == null || item.icon!.isEmpty) 
                    ? const Icon(Icons.apps, color: PiccoloTheme.inkMuted)
                    : null,
                ),
                const SizedBox(width: 12),
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
                      const SizedBox(height: 4),
                      Text(
                        "${item.category} • v${item.version}",
                        style: PiccoloTheme.textTheme.labelSmall?.copyWith(
                          color: PiccoloTheme.inkMuted,
                        ),
                      ),
                    ],
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
                maxLines: 3,
                overflow: TextOverflow.ellipsis,
              ),
            ),
             const SizedBox(height: 8),
             if (item.tags.isNotEmpty)
               Wrap(
                 spacing: 4,
                 runSpacing: 4,
                 children: item.tags.take(3).map((tag) => Container(
                   padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 2),
                   decoration: BoxDecoration(
                     color: Colors.black.withValues(alpha: 0.05),
                     borderRadius: BorderRadius.circular(4),
                   ),
                   child: Text(tag, style: PiccoloTheme.textTheme.labelSmall?.copyWith(fontSize: 10)),
                 )).toList(),
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
