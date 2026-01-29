import 'package:flutter/material.dart';
import '../../theme/piccolo_theme.dart';
import '../../shells/desktop/desktop_controller.dart';
import 'store_tab.dart';
import 'custom_install_wizard.dart';
import 'create_workspace_wizard.dart';
import 'app_detail_view.dart';

class AppStoreWindow extends StatefulWidget {
  final DesktopController desktopController;

  const AppStoreWindow({super.key, required this.desktopController});

  @override
  State<AppStoreWindow> createState() => _AppStoreWindowState();
}

class _AppStoreWindowState extends State<AppStoreWindow> {
  final TextEditingController _searchController = TextEditingController();
  String _searchQuery = "";

  // Category state (managed here, passed to StoreTab)
  List<String> _categories = ['All'];
  String _selectedCategory = 'All';

  @override
  void initState() {
    super.initState();
    _searchController.addListener(() {
      setState(() {
        _searchQuery = _searchController.text;
      });
    });
    _loadCategories();
  }

  @override
  void dispose() {
    _searchController.dispose();
    super.dispose();
  }

  Future<void> _loadCategories() async {
    try {
      final cats = await widget.desktopController.appService.getCategories();
      if (!mounted) return;
      setState(() {
        _categories = ['All', ...cats];
      });
    } catch (e) {
      debugPrint("Failed to load categories: $e");
    }
  }

  void _onCategorySelected(String category) {
    if (_selectedCategory == category) return;
    setState(() {
      _selectedCategory = category;
    });
  }

  void _openCustomInstallWizard() {
    showDialog(
      context: context,
      barrierDismissible: false,
      builder: (context) => CustomInstallWizard(
        appService: widget.desktopController.appService,
        onSuccess: (appName) {
          Navigator.of(context).pop();
          _openAppDetail(appName);
        },
      ),
    );
  }

  void _openCreateWorkspace() {
    showDialog(
      context: context,
      barrierDismissible: false,
      builder: (context) => CreateWorkspaceWizard(
        appService: widget.desktopController.appService,
        onSuccess: () {},
      ),
    );
  }

  void _openAppDetail(String appName, {String? iconUrl}) {
    widget.desktopController.notifyAppsChanged();
    widget.desktopController.openApp(
      "app-detail-$appName",
      appName,
      Icons.settings_applications,
      AppDetailView(
        appId: appName,
        appService: widget.desktopController.appService,
        desktopController: widget.desktopController,
        iconUrl: iconUrl,
      ),
      iconUrl: iconUrl,
    );
  }

  @override
  Widget build(BuildContext context) {
    return Column(
      children: [
        // Header Row 1: Search + Action Buttons
        Container(
          padding: const EdgeInsets.fromLTRB(16, 12, 16, 8),
          decoration: const BoxDecoration(
            color: PiccoloTheme.porcelain,
            border: Border(
              bottom: BorderSide(color: Colors.black12),
            ),
          ),
          child: Row(
            children: [
              // Search Bar (expanded)
              Expanded(
                child: TextField(
                  controller: _searchController,
                  decoration: InputDecoration(
                    hintText: 'Search apps...',
                    prefixIcon: const Icon(Icons.search, size: 20),
                    contentPadding: const EdgeInsets.symmetric(
                      vertical: 10,
                      horizontal: 12,
                    ),
                    border: OutlineInputBorder(
                      borderRadius: BorderRadius.circular(8),
                      borderSide: BorderSide.none,
                    ),
                    filled: true,
                    fillColor: PiccoloTheme.mist,
                  ),
                  style: const TextStyle(fontSize: 14),
                ),
              ),
              const SizedBox(width: 16),
              // Create Workspace Button
              OutlinedButton.icon(
                onPressed: _openCreateWorkspace,
                icon: const Icon(Icons.terminal, size: 18),
                label: const Text("Create Workspace"),
                style: OutlinedButton.styleFrom(
                  foregroundColor: PiccoloTheme.ink,
                  side: const BorderSide(color: Colors.black26),
                  padding: const EdgeInsets.symmetric(
                    horizontal: 16,
                    vertical: 12,
                  ),
                ),
              ),
              const SizedBox(width: 8),
              // Custom App Button
              OutlinedButton.icon(
                onPressed: _openCustomInstallWizard,
                icon: const Icon(Icons.add_box_outlined, size: 18),
                label: const Text("Custom App"),
                style: OutlinedButton.styleFrom(
                  foregroundColor: PiccoloTheme.ink,
                  side: const BorderSide(color: Colors.black26),
                  padding: const EdgeInsets.symmetric(
                    horizontal: 16,
                    vertical: 12,
                  ),
                ),
              ),
            ],
          ),
        ),

        // Header Row 2: Category Chips (wrapping for mouse-friendly interaction)
        Container(
          width: double.infinity,
          padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 8),
          decoration: const BoxDecoration(
            color: PiccoloTheme.porcelain,
            border: Border(
              bottom: BorderSide(color: Colors.black12),
            ),
          ),
          child: Wrap(
            spacing: 8,
            runSpacing: 8,
            children: [
              for (final cat in _categories)
                ChoiceChip(
                  label: Text(cat),
                  selected: cat == _selectedCategory,
                  showCheckmark: false,
                  onSelected: (_) => _onCategorySelected(cat),
                  selectedColor: PiccoloTheme.cobalt600,
                  labelStyle: TextStyle(
                    color: cat == _selectedCategory
                        ? Colors.white
                        : PiccoloTheme.ink,
                    fontWeight: cat == _selectedCategory
                        ? FontWeight.bold
                        : FontWeight.normal,
                  ),
                  backgroundColor: Colors.white,
                  side: BorderSide(
                    color: cat == _selectedCategory
                        ? PiccoloTheme.cobalt600
                        : Colors.black26,
                  ),
                  shape: RoundedRectangleBorder(
                    borderRadius: BorderRadius.circular(20),
                  ),
                ),
            ],
          ),
        ),

        // Content: Store Tab (catalog grid)
        Expanded(
          child: StoreTab(
            appService: widget.desktopController.appService,
            searchQuery: _searchQuery,
            desktopController: widget.desktopController,
            selectedCategory: _selectedCategory,
          ),
        ),
      ],
    );
  }
}
