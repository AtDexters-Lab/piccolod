import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/features/apps/app_detail_view.dart';
import 'package:piccolo_os/features/apps/create_workspace_wizard.dart';
import 'package:piccolo_os/features/apps/custom_install_wizard.dart';
import 'package:piccolo_os/features/apps/store_tab.dart';
import 'package:piccolo_os/shells/desktop/desktop_controller.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class AppStoreWindow extends StatefulWidget {

  const AppStoreWindow({required this.desktopController, super.key});
  final DesktopController desktopController;

  @override
  State<AppStoreWindow> createState() => _AppStoreWindowState();
}

class _AppStoreWindowState extends State<AppStoreWindow> {
  final TextEditingController _searchController = TextEditingController();
  String _searchQuery = '';

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
    unawaited(_loadCategories());
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
    } on Object catch (e) {
      debugPrint('Failed to load categories: $e');
    }
  }

  void _onCategorySelected(String category) {
    if (_selectedCategory == category) return;
    setState(() {
      _selectedCategory = category;
    });
  }

  void _openCustomInstallWizard() {
    unawaited(showDialog<void>(
      context: context,
      barrierDismissible: false,
      builder: (context) => CustomInstallWizard(
        appService: widget.desktopController.appService,
        onSuccess: (appName) {
          Navigator.of(context).pop();
          _openAppDetail(appName);
        },
      ),
    ));
  }

  void _openCreateWorkspace() {
    unawaited(showDialog<void>(
      context: context,
      barrierDismissible: false,
      builder: (context) => CreateWorkspaceWizard(
        appService: widget.desktopController.appService,
        onSuccess: () {},
      ),
    ));
  }

  void _openAppDetail(String appName, {String? iconUrl}) {
    widget.desktopController.notifyAppsChanged();
    widget.desktopController.openApp(
      'app-detail-$appName',
      appName,
      PiccoloIcons.settingsApp,
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
          padding: const EdgeInsets.fromLTRB(Spacing.base, Spacing.md, Spacing.base, Spacing.sm),
          decoration: const BoxDecoration(
            color: PiccoloTheme.porcelain,
            border: Border(
              bottom: BorderSide(color: PiccoloTheme.hairline),
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
                    prefixIcon: const Icon(PiccoloIcons.search, size: 20),
                    contentPadding: const EdgeInsets.symmetric(
                      vertical: Spacing.sm,
                      horizontal: Spacing.md,
                    ),
                    border: OutlineInputBorder(
                      borderRadius: BorderRadius.circular(Radii.sm),
                      borderSide: BorderSide.none,
                    ),
                    filled: true,
                    fillColor: PiccoloTheme.mist,
                  ),
                  style: const TextStyle(fontSize: 14),
                ),
              ),
              const SizedBox(width: Spacing.base),
              // Create Workspace Button
              OutlinedButton.icon(
                onPressed: _openCreateWorkspace,
                icon: const Icon(PiccoloIcons.terminal, size: 18),
                label: const Text('Create Workspace'),
              ),
              const SizedBox(width: Spacing.sm),
              // Custom App Button
              OutlinedButton.icon(
                onPressed: _openCustomInstallWizard,
                icon: const Icon(PiccoloIcons.addBox, size: 18),
                label: const Text('Custom App'),
              ),
            ],
          ),
        ),

        // Header Row 2: Category Chips (wrapping for mouse-friendly interaction)
        Container(
          width: double.infinity,
          padding: const EdgeInsets.symmetric(horizontal: Spacing.base, vertical: Spacing.sm),
          decoration: const BoxDecoration(
            color: PiccoloTheme.porcelain,
            border: Border(
              bottom: BorderSide(color: PiccoloTheme.hairline),
            ),
          ),
          child: Wrap(
            spacing: Spacing.sm,
            runSpacing: Spacing.sm,
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
                  backgroundColor: PiccoloTheme.porcelain,
                  side: BorderSide(
                    color: cat == _selectedCategory
                        ? PiccoloTheme.cobalt600
                        : PiccoloTheme.outline,
                  ),
                  shape: RoundedRectangleBorder(
                    borderRadius: BorderRadius.circular(Radii.lg),
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
