import 'package:flutter/material.dart';
import '../../theme/piccolo_theme.dart';
import '../../core/models/app_models.dart';
import '../../core/services/app_service.dart';
import '../../shells/desktop/desktop_controller.dart';
import 'app_launcher.dart';

class AppDetailView extends StatefulWidget {
  final String appId;
  final AppService appService;
  final DesktopController desktopController;

  const AppDetailView({
    super.key,
    required this.appId,
    required this.appService,
    required this.desktopController,
  });

  @override
  State<AppDetailView> createState() => _AppDetailViewState();
}

class _AppDetailViewState extends State<AppDetailView> with SingleTickerProviderStateMixin {
  late TabController _tabController;
  
  App? _app;
  List<ServiceEndpoint> _services = [];
  bool _isLoading = true;
  String? _error;
  
  // Action states
  bool _isActionLoading = false;

  @override
  void initState() {
    super.initState();
    _tabController = TabController(length: 3, vsync: this);
    _loadData();
  }
  
  @override
  void dispose() {
    _tabController.dispose();
    super.dispose();
  }

  Future<void> _loadData() async {
    setState(() {
      _isLoading = true;
      _error = null;
    });

    try {
      final app = await widget.appService.getAppDetail(widget.appId);
      final services = await widget.appService.getAppServices(widget.appId);
      
      if (!mounted) return;

      setState(() {
        _app = app;
        _services = services;
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

  Future<void> _handleAction(Future<void> Function() action) async {
    setState(() => _isActionLoading = true);
    try {
      await action();
      // Delay slightly to allow backend state to propagate/Podman to react
      await Future.delayed(const Duration(seconds: 1));
      if (!mounted) return;
      await _loadData(); // Refresh UI
    } catch (e) {
      if (mounted) {
        ScaffoldMessenger.of(context).showSnackBar(SnackBar(content: Text("Action failed: $e")));
      }
    } finally {
      if (mounted) setState(() => _isActionLoading = false);
    }
  }

  void _confirmUninstall() {
    bool purgeData = false;
    
    showDialog(
      context: context,
      builder: (dialogContext) => StatefulBuilder(
        builder: (context, setDialogState) {
          return AlertDialog(
            title: Text("Uninstall ${_app?.name}?"),
            content: Column(
              mainAxisSize: MainAxisSize.min,
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                const Text("This will remove the application container."),
                const SizedBox(height: 16),
                Container(
                  decoration: BoxDecoration(
                    color: PiccoloTheme.critical.withValues(alpha: 0.1),
                    borderRadius: BorderRadius.circular(8),
                    border: Border.all(color: PiccoloTheme.critical.withValues(alpha: 0.3)),
                  ),
                  child: CheckboxListTile(
                    title: const Text(
                      "Delete Data Volumes",
                      style: TextStyle(color: PiccoloTheme.critical, fontWeight: FontWeight.bold),
                    ),
                    subtitle: const Text("This action cannot be undone."),
                    value: purgeData,
                    activeColor: PiccoloTheme.critical,
                    onChanged: (val) => setDialogState(() => purgeData = val ?? false),
                    controlAffinity: ListTileControlAffinity.leading,
                    contentPadding: const EdgeInsets.symmetric(horizontal: 8),
                  ),
                ),
              ],
            ),
            actions: [
              TextButton(
                onPressed: () => Navigator.of(dialogContext).pop(),
                child: const Text("Cancel"),
              ),
              FilledButton(
                style: FilledButton.styleFrom(backgroundColor: PiccoloTheme.critical),
                onPressed: () async {
                  Navigator.of(dialogContext).pop(); // Close dialog
                  
                  // Use outer setState to trigger main view rebuild
                  setState(() => _isActionLoading = true);
                  final messenger = ScaffoldMessenger.of(context);
                  
                  try {
                     await widget.appService.uninstallApp(widget.appId, purge: purgeData);
                     
                     if (mounted) {
                       setState(() {
                         _app = null; // Forces "App uninstalled" state
                         _isActionLoading = false;
                       });
                     }
                  } catch (e) {
                     if (mounted) {
                        setState(() => _isActionLoading = false);
                        messenger.showSnackBar(
                           SnackBar(content: Text("Uninstall failed: $e")),
                        );
                     }
                  }
                },
                child: const Text("Uninstall"),
              ),
            ],
          );
        },
      ),
    );
  }

  @override
  Widget build(BuildContext context) {
    // Removed Scaffold/AppBar to fit into WindowFrame naturally
    return _buildBody();
  }

  Widget _buildBody() {
    if (_isLoading) return const Center(child: CircularProgressIndicator());
    if (_error != null) return Center(child: Text("Error: $_error"));
    if (_app == null) return const Center(child: Text("App uninstalled."));

    return Column(
      children: [
        // Header
        _buildHeader(),
        
        // Tabs
        TabBar(
          controller: _tabController,
          labelColor: PiccoloTheme.cobalt600,
          unselectedLabelColor: PiccoloTheme.inkMuted,
          indicatorColor: PiccoloTheme.cobalt600,
          tabs: const [
            Tab(text: "Overview"),
            Tab(text: "Network"),
            Tab(text: "Configuration"),
          ],
        ),
        
        // Content
        Expanded(
          child: Container(
            color: PiccoloTheme.mist,
            child: TabBarView(
              controller: _tabController,
              children: [
                _buildOverviewTab(),
                _buildNetworkTab(),
                _buildConfigTab(),
              ],
            ),
          ),
        ),
      ],
    );
  }

  Widget _buildHeader() {
    Color statusColor = PiccoloTheme.inkMuted;
    if (_app!.isRunning) statusColor = PiccoloTheme.success;
    if (_app!.isError) statusColor = PiccoloTheme.critical;

    return Container(
      color: Colors.white,
      padding: const EdgeInsets.all(24.0),
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          // Icon
          Container(
            width: 80,
            height: 80,
            decoration: BoxDecoration(
              color: PiccoloTheme.mist,
              borderRadius: BorderRadius.circular(16),
            ),
            child: Center(
              child: Text(
                _app!.name[0].toUpperCase(),
                style: const TextStyle(fontSize: 32, fontWeight: FontWeight.bold, color: PiccoloTheme.cobalt600),
              ),
            ),
          ),
          const SizedBox(width: 24),
          
          // Info
          Expanded(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Row(
                  children: [
                    Text(_app!.name, style: PiccoloTheme.textTheme.displayLarge?.copyWith(fontSize: 24)),
                    const Spacer(),
                    // Uninstall Button (Moved here from AppBar)
                    IconButton(
                      icon: const Icon(Icons.delete_outline, color: PiccoloTheme.critical),
                      onPressed: _confirmUninstall,
                      tooltip: "Uninstall",
                    ),
                  ],
                ),
                const SizedBox(height: 8),
                Row(
                  children: [
                    Container(
                      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 4),
                      decoration: BoxDecoration(
                        color: statusColor.withValues(alpha: 0.1),
                        borderRadius: BorderRadius.circular(12),
                      ),
                      child: Row(
                        mainAxisSize: MainAxisSize.min,
                        children: [
                          Container(
                            width: 8,
                            height: 8,
                            decoration: BoxDecoration(color: statusColor, shape: BoxShape.circle),
                          ),
                          const SizedBox(width: 6),
                          Text(
                            _app!.status.toUpperCase(),
                            style: TextStyle(color: statusColor, fontSize: 12, fontWeight: FontWeight.bold),
                          ),
                        ],
                      ),
                    ),
                    const SizedBox(width: 16),
                    Text("Image: ${_app!.image}", style: PiccoloTheme.textTheme.labelSmall),
                  ],
                ),
              ],
            ),
          ),
          
          const SizedBox(width: 24),

          // Actions
          if (_isActionLoading)
             const CircularProgressIndicator()
          else if (_app!.isRunning)
             FilledButton.icon(
               onPressed: () => _handleAction(() => widget.appService.stopApp(_app!.name)),
               icon: const Icon(Icons.stop),
               label: const Text("Stop"),
               style: FilledButton.styleFrom(backgroundColor: PiccoloTheme.inkMuted),
             )
          else
             FilledButton.icon(
               onPressed: () => _handleAction(() => widget.appService.startApp(_app!.name)),
               icon: const Icon(Icons.play_arrow),
               label: const Text("Start"),
               style: FilledButton.styleFrom(backgroundColor: PiccoloTheme.success),
             ),
        ],
      ),
    );
  }

  Widget _buildOverviewTab() {
    return ListView(
      padding: const EdgeInsets.all(24),
      children: [
        _buildSectionTitle("Storage Volumes"),
        if (_app!.volumes.isEmpty)
          const Text("No persistent volumes configured.")
        else
          ..._app!.volumes.map((v) => Card(
            child: ListTile(
              leading: const Icon(Icons.storage),
              title: Text(v.containerPath),
              subtitle: Text("Host: ${v.hostPath}"),
              trailing: Text(v.sizeLimit),
            ),
          )),
          
        const SizedBox(height: 24),
        _buildSectionTitle("Environment Variables"),
        if (_app!.environment.isEmpty)
           const Text("No environment variables.")
        else
           Container(
             padding: const EdgeInsets.all(16),
             decoration: BoxDecoration(
               color: Colors.white,
               borderRadius: BorderRadius.circular(8),
               border: Border.all(color: Colors.black12),
             ),
             child: Column(
               crossAxisAlignment: CrossAxisAlignment.start,
               children: _app!.environment.entries.map((e) => Padding(
                 padding: const EdgeInsets.symmetric(vertical: 4),
                 child: Row(
                   crossAxisAlignment: CrossAxisAlignment.start,
                   children: [
                     SizedBox(
                       width: 120, 
                       child: Text(e.key, style: const TextStyle(fontWeight: FontWeight.bold, fontFamily: 'JetBrainsMono', fontSize: 12)),
                     ),
                     Expanded(
                       child: Text(e.value, style: const TextStyle(fontFamily: 'JetBrainsMono', fontSize: 12)),
                     ),
                   ],
                 ),
               )).toList(),
             ),
           ),
      ],
    );
  }

  Widget _buildNetworkTab() {
     if (_services.isEmpty) {
       return const Center(child: Text("No network services exposed."));
     }
     
     return ListView(
       padding: const EdgeInsets.all(24),
       children: _services.map((svc) => Card(
         child: Padding(
           padding: const EdgeInsets.all(16.0),
           child: Column(
             crossAxisAlignment: CrossAxisAlignment.start,
             children: [
               Row(
                 children: [
                   const Icon(Icons.router, color: PiccoloTheme.cobalt600),
                   const SizedBox(width: 12),
                   Text(svc.name, style: const TextStyle(fontWeight: FontWeight.bold, fontSize: 16)),
                   const Spacer(),
                   Container(
                     padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 2),
                     decoration: BoxDecoration(
                       color: PiccoloTheme.mist,
                       borderRadius: BorderRadius.circular(4),
                       border: Border.all(color: Colors.black12),
                     ),
                     child: Text(svc.protocol.toUpperCase(), style: const TextStyle(fontSize: 12)),
                   ),
                 ],
               ),
               const Divider(height: 24),
               _buildNetworkRow("Internal Port", "${svc.guestPort}"),
               _buildNetworkRow("LAN Access", "${svc.lanUrl} (Port ${svc.publicPort})"),
               
               if (svc.remoteUrl != null)
                  _buildNetworkRow("Remote Access", svc.remoteUrl!),
                  
               const SizedBox(height: 16),
               Row(
                 children: [
                   Expanded(
                     child: OutlinedButton.icon(
                       onPressed: () => AppLauncher.openAppWindow(
                         controller: widget.desktopController,
                         appService: widget.appService,
                         app: _app!,
                         service: svc,
                       ),
                       icon: const Icon(Icons.lan, size: 16),
                       label: const Text("Open Local"),
                     ),
                   ),
                   if (svc.remoteUrl != null) ...[
                     const SizedBox(width: 12),
                     Expanded(
                       child: FilledButton.icon(
                         onPressed: () => AppLauncher.openAppWindow(
                           controller: widget.desktopController,
                           appService: widget.appService,
                           app: _app!,
                           service: svc,
                           overrideUrl: svc.remoteUrl!,
                         ),
                         icon: const Icon(Icons.public, size: 16),
                         label: const Text("Open Remote"),
                         style: FilledButton.styleFrom(backgroundColor: PiccoloTheme.cobalt600),
                       ),
                     ),
                   ],
                 ],
               ),
             ],
           ),
         ),
       )).toList(),
     );
  }

  Widget _buildConfigTab() {
    // Ideally this would show the original YAML, but the API doesn't return it yet.
    // We can just show a JSON dump of the App object for now.
    return Padding(
      padding: const EdgeInsets.all(24.0),
      child: Container(
        padding: const EdgeInsets.all(16),
        decoration: BoxDecoration(
          color: Colors.white,
          borderRadius: BorderRadius.circular(8),
          border: Border.all(color: Colors.black12),
        ),
        child: SelectableText(
          "App ID: ${_app!.id}\n"
          "Type: ${_app!.type}\n"
          "Container ID: ${_app!.containerId}\n",
          style: const TextStyle(fontFamily: 'JetBrainsMono'),
        ),
      ),
    );
  }
  
  Widget _buildSectionTitle(String title) {
    return Padding(
      padding: const EdgeInsets.only(bottom: 12),
      child: Text(title, style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
    );
  }
  
  Widget _buildNetworkRow(String label, String value) {
    return Padding(
      padding: const EdgeInsets.symmetric(vertical: 4),
      child: Row(
        children: [
          SizedBox(width: 120, child: Text(label, style: const TextStyle(color: PiccoloTheme.inkMuted))),
          Expanded(child: SelectableText(value, style: const TextStyle(fontFamily: 'JetBrainsMono'))),
        ],
      ),
    );
  }
}