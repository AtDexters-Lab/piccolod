import 'package:flutter/material.dart';
import '../../theme/piccolo_theme.dart';
import '../../core/services/app_service.dart';
import '../../core/services/api_client.dart';
import '../../shells/desktop/desktop_controller.dart';
import 'library_tab.dart';
import 'store_tab.dart';
import 'custom_install_wizard.dart';
import 'app_detail_view.dart';

class AppStoreWindow extends StatefulWidget {
  final DesktopController desktopController;

  const AppStoreWindow({super.key, required this.desktopController});

  @override
  State<AppStoreWindow> createState() => _AppStoreWindowState();
}

class _AppStoreWindowState extends State<AppStoreWindow>
    with SingleTickerProviderStateMixin {
  late TabController _tabController;
  final TextEditingController _searchController = TextEditingController();
  String _searchQuery = "";
  
  final AppService _appService = AppService(ApiClient());

  @override
  void initState() {
    super.initState();
    _tabController = TabController(length: 2, vsync: this);
    _searchController.addListener(() {
      setState(() {
        _searchQuery = _searchController.text;
      });
    });
  }

  @override
  void dispose() {
    _tabController.dispose();
    _searchController.dispose();
    super.dispose();
  }

  void _openCustomInstallWizard() {
    showDialog(
      context: context,
      barrierDismissible: false,
      builder: (context) => CustomInstallWizard(
        appService: _appService,
        onSuccess: (appName) {
           Navigator.of(context).pop(); // Close Wizard
           // Open App Detail Window
           // We need to fetch basic app info first to open the window, OR just open it and let DetailView fetch.
           // DetailView needs AppService.
           // We can't easily open a window from here without DesktopController having a method for AppDetailView.
           // But wait, AppDetailView IS a Widget. We can just push it?
           // No, user wants "App Page... in a new window".
           
           // So we use DesktopController to open a new window with AppDetailView content.
           widget.desktopController.openApp(
             "app-detail-$appName",
             appName,
             Icons.settings_applications, // Placeholder icon
             AppDetailView(
               appId: appName,
               appService: _appService,
               desktopController: widget.desktopController,
             ),
           );
        },
      ),
    );
  }

  @override
  Widget build(BuildContext context) {
    return Column(
      children: [
        // Header / Toolbar
        Container(
          height: 60,
          padding: const EdgeInsets.symmetric(horizontal: 16),
          decoration: const BoxDecoration(
            color: PiccoloTheme.porcelain,
            border: Border(
              bottom: BorderSide(color: Colors.black12),
            ),
          ),
          child: Row(
            children: [
              // Tabs
              Expanded(
                child: TabBar(
                  controller: _tabController,
                  isScrollable: true,
                  labelColor: PiccoloTheme.cobalt600,
                  unselectedLabelColor: PiccoloTheme.inkMuted,
                  indicatorColor: PiccoloTheme.cobalt600,
                  indicatorSize: TabBarIndicatorSize.label,
                  tabs: const [
                    Tab(text: "Library"),
                    Tab(text: "Store"),
                  ],
                ),
              ),
              const SizedBox(width: 16),
              // Search Bar
              SizedBox(
                width: 240,
                child: TextField(
                  controller: _searchController,
                  decoration: InputDecoration(
                    hintText: 'Search catalog...',
                    prefixIcon: const Icon(Icons.search, size: 18),
                    contentPadding: const EdgeInsets.symmetric(
                      vertical: 8,
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
              // Custom Install Button
              FilledButton.icon(
                onPressed: _openCustomInstallWizard,
                icon: const Icon(Icons.add_box_outlined, size: 18),
                label: const Text("Custom App"),
                style: FilledButton.styleFrom(
                  backgroundColor: PiccoloTheme.cobalt600,
                  foregroundColor: Colors.white,
                  padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 0),
                ),
              ),
            ],
          ),
        ),

        // Content
        Expanded(
          child: TabBarView(
            controller: _tabController,
            children: [
              // Library Tab
              LibraryTab(
                appService: _appService,
                searchQuery: _searchQuery,
                desktopController: widget.desktopController,
              ),
              // Store Tab
              StoreTab(
                appService: _appService,
                searchQuery: _searchQuery,
                onInstallCustom: _openCustomInstallWizard,
                desktopController: widget.desktopController,
              ),
            ],
          ),
        ),
      ],
    );
  }
}
