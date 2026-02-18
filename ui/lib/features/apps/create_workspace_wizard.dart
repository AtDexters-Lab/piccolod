import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/config/core_config.dart';
import 'package:piccolo_os/core/models/app_models.dart';
import 'package:piccolo_os/core/models/task_progress.dart';
import 'package:piccolo_os/core/services/app_service.dart';
import 'package:piccolo_os/core/utils/task_id.dart';
import 'package:piccolo_os/features/apps/dynamic_install_wizard.dart';
import 'package:piccolo_os/shared/widgets/app_icon.dart';
import 'package:piccolo_os/shared/widgets/task_progress_panel.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Wizard dialog for creating a new Workspace container.
///
/// Workspaces are persistent, mutable containers that behave like lightweight VPS.
/// This wizard allows users to:
/// 1. Select a base image (from catalog or Docker Hub search)
/// 2. Configure the workspace (for catalog: use DynamicInstallWizard; for custom: inline config)
/// 3. Watch the installation progress
class CreateWorkspaceWizard extends StatefulWidget {

  const CreateWorkspaceWizard({
    required this.appService, super.key,
    this.onSuccess,
  });
  final AppService appService;
  final VoidCallback? onSuccess;

  @override
  State<CreateWorkspaceWizard> createState() => _CreateWorkspaceWizardState();
}

class _CreateWorkspaceWizardState extends State<CreateWorkspaceWizard> {
  int _currentStep =
      0; // 0: Select Image, 1: Configure (custom only), 2: Installing

  // Catalog workspaces (loaded from store)
  List<CatalogItem> _catalogWorkspaces = [];
  bool _isLoadingCatalog = true;

  // Search state (for custom Docker Hub images)
  final TextEditingController _searchController = TextEditingController();
  List<ImageSearchResult> _searchResults = [];
  bool _isSearching = false;

  // Selection (for custom image flow)
  String? _selectedImage;
  String? _selectedImageName;
  // RFC 20260130: workspace_name is the identity for workspace mode apps
  final TextEditingController _workspaceNameController = TextEditingController();
  final TextEditingController _tagController = TextEditingController(
    text: 'latest',
  );
  final GlobalKey<FormState> _formKey = GlobalKey<FormState>();

  // Install state (for custom image flow)
  bool _isInstalling = false;
  String? _taskId;
  String? _error;

  @override
  void initState() {
    super.initState();
    unawaited(_loadCatalogWorkspaces());
  }

  @override
  void dispose() {
    _searchController.dispose();
    _workspaceNameController.dispose();
    _tagController.dispose();
    super.dispose();
  }

  Future<void> _loadCatalogWorkspaces() async {
    try {
      final response = await widget.appService.getCatalog(
        category: 'Workspace',
      );
      if (!mounted) return;
      setState(() {
        _catalogWorkspaces = response.apps;
        _isLoadingCatalog = false;
      });
    } on Object catch (e) {
      if (!mounted) return;
      setState(() {
        _isLoadingCatalog = false;
        // Silently fail - still allow custom image search
        debugPrint('Failed to load workspace catalog: $e');
      });
    }
  }

  Future<void> _searchImages() async {
    final query = _searchController.text.trim();
    if (query.isEmpty) return;

    setState(() {
      _isSearching = true;
      _error = null;
    });

    try {
      final results = await widget.appService.searchImages(query);
      setState(() {
        _searchResults = results;
        _isSearching = false;
      });
    } on Object catch (e) {
      setState(() {
        _error = e.toString();
        _isSearching = false;
      });
    }
  }

  Future<void> _installFromCatalog(CatalogItem item) async {
    // Capture references before any async operations or navigation changes
    final appService = widget.appService;
    final onSuccess = widget.onSuccess;
    final navigator = Navigator.of(context);

    // Fetch template and schema before closing this wizard
    var yaml = item.template;
    if (yaml == null || yaml.isEmpty) {
      try {
        yaml = await appService.getCatalogTemplate(item.name);
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

    // Fetch configuration schema
    var schema = <String, dynamic>{};
    try {
      schema = await appService.getCatalogConfigure(item.name);
    } on Object catch (e) {
      debugPrint('Failed to load config schema: $e');
    }

    if (!mounted) return;
    // Show DynamicInstallWizard first (while context is still valid), then pop this wizard.
    unawaited(showDialog<bool>(
      context: context,
      barrierDismissible: false,
      builder: (dialogContext) => DynamicInstallWizard(
        appService: appService,
        appName: item.name,
        yamlContent: yaml!,
        schema: schema,
        onSuccess: (appName) {
          Navigator.of(dialogContext).pop(true);
          onSuccess?.call();
        },
      ),
    ).then((done) {
      if ((done ?? false) && mounted) {
        navigator.pop();
      }
    }));
  }

  void _selectCustomImage(String image, String displayName) {
    // Extract base image name without tag
    final parts = image.split(':');
    final baseImage = parts.first;
    final tag = parts.length > 1 ? parts[1] : 'latest';

    setState(() {
      _selectedImage = baseImage;
      _selectedImageName = displayName;
      _tagController.text = tag;
      // RFC 20260130: Pre-fill workspace name from image, user can modify
      _workspaceNameController.text = _workspaceNameFromImage(baseImage);
      _currentStep = 1;
    });
  }

  String _workspaceNameFromImage(String image) {
    // RFC 20260130: workspace_name must be lowercase letters and numbers only (no hyphens)
    // Extract base name: library/nginx -> nginx, nginx:alpine -> nginx
    var name = image.split('/').last;
    if (name.contains(':')) {
      name = name.split(':').first;
    }
    // Remove underscores and hyphens (RFC 20260130 constraints)
    name = name.toLowerCase().replaceAll(RegExp('[_-]'), '');
    // Truncate to 16 chars max
    if (name.length > 16) {
      name = name.substring(0, 16);
    }
    return name;
  }

  String _generateWorkspaceManifest(String image) {
    // RFC 20260130: workspace_name is the identity for workspace mode apps without listeners
    final workspaceName = _workspaceNameController.text.trim().isNotEmpty
        ? _workspaceNameController.text.trim()
        : _workspaceNameFromImage(image);

    // Custom images get no default listeners - user adds via Edit Listeners
    return '''
workspace_name: $workspaceName
type: user
services:
  main:
    image: $image
    bind_ports: []
x-piccolo:
  mode: workspace
''';
  }

  Future<void> _createCustomWorkspace() async {
    if (_selectedImage == null) return;

    // Validate form before proceeding
    if (!(_formKey.currentState?.validate() ?? false)) {
      return;
    }

    final taskId = generateTaskId();
    final tag = _tagController.text.trim().isNotEmpty
        ? _tagController.text.trim()
        : 'latest';
    final image = '$_selectedImage:$tag';

    setState(() {
      _isInstalling = true;
      _taskId = taskId;
      _currentStep = 2;
      _error = null;
    });

    try {
      final yaml = _generateWorkspaceManifest(image);

      await widget.appService.initiateInstall(
        yaml,
        {},
        taskId: taskId,
      );
    } on Object catch (e) {
      if (mounted) {
        setState(() {
          _isInstalling = false;
          _taskId = null;
          _error = e.toString();
          _currentStep = 1;
        });
      }
    }
  }

  void _onInstallComplete(TaskProgressEvent event) {
    if (!mounted) return;
    if (event.error != null && event.error!.isNotEmpty) {
      setState(() {
        _isInstalling = false;
        _taskId = null;
        _error = event.error;
        _currentStep = 1;
      });
      return;
    }
    widget.onSuccess?.call();
  }

  @override
  Widget build(BuildContext context) {
    return Dialog(
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(Radii.lg)),
      child: Container(
        width: 700,
        height: 600,
        decoration: BoxDecoration(
          color: PiccoloTheme.porcelain,
          borderRadius: BorderRadius.circular(Radii.lg),
        ),
        child: Column(
          children: [
            _buildHeader(),
            Expanded(child: _buildBody()),
            _buildFooter(),
          ],
        ),
      ),
    );
  }

  Widget _buildHeader() {
    final titles = [
      'Select Base Image',
      'Configure Workspace',
      'Creating Workspace',
    ];

    return Container(
      padding: const EdgeInsets.all(Spacing.lg),
      decoration: const BoxDecoration(
        border: Border(bottom: BorderSide(color: PiccoloTheme.hairline)),
      ),
      child: Row(
        children: [
          const Icon(PiccoloIcons.terminal, color: PiccoloTheme.cobalt600, size: 32),
          const SizedBox(width: Spacing.base),
          Expanded(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Text(
                  'Create Workspace',
                  style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
                    fontWeight: FontWeight.bold,
                  ),
                ),
                Text(
                  titles[_currentStep],
                  style: PiccoloTheme.textTheme.bodyMedium?.copyWith(
                    color: PiccoloTheme.inkMuted,
                  ),
                ),
              ],
            ),
          ),
          IconButton(
            icon: const Icon(PiccoloIcons.close),
            onPressed: _isInstalling ? null : () => Navigator.of(context).pop(),
          ),
        ],
      ),
    );
  }

  Widget _buildBody() {
    if (_currentStep == 2 && _taskId != null) {
      return Padding(
        padding: const EdgeInsets.all(Spacing.lg),
        child: TaskProgressPanel(
          taskId: _taskId!,
          taskType: 'install_app',
          onComplete: _onInstallComplete,
        ),
      );
    }

    if (_currentStep == 1) {
      return _buildConfigureStep();
    }

    return _buildImageSelectionStep();
  }

  Widget _buildImageSelectionStep() {
    return SingleChildScrollView(
      padding: const EdgeInsets.all(Spacing.lg),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          // Catalog workspaces section
          Text(
            'Featured Workspaces',
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
              fontWeight: FontWeight.w600,
            ),
          ),
          const SizedBox(height: Spacing.base),
          if (_isLoadingCatalog)
            const Center(child: CircularProgressIndicator())
          else if (_catalogWorkspaces.isEmpty)
            Container(
              padding: const EdgeInsets.all(Spacing.base),
              decoration: BoxDecoration(
                color: PiccoloTheme.mist,
                borderRadius: BorderRadius.circular(Radii.sm),
              ),
              child: const Text(
                'No workspace templates available. Use Docker Hub search below.',
              ),
            )
          else
            Wrap(
              spacing: Spacing.md,
              runSpacing: Spacing.md,
              children: _catalogWorkspaces
                  .map(_buildCatalogCard)
                  .toList(),
            ),

          const SizedBox(height: Spacing.xl),

          // Custom image section
          Text(
            'Custom Image (Docker Hub)',
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
              fontWeight: FontWeight.w600,
            ),
          ),
          const SizedBox(height: Spacing.sm),
          Text(
            'Search Docker Hub for any container image to use as a workspace base.',
            style: PiccoloTheme.textTheme.bodySmall?.copyWith(
              color: PiccoloTheme.inkMuted,
            ),
          ),
          const SizedBox(height: Spacing.base),
          Row(
            children: [
              Expanded(
                child: TextField(
                  controller: _searchController,
                  decoration: const InputDecoration(
                    hintText: 'Search for images (e.g., postgres, redis)...',
                    border: OutlineInputBorder(),
                    filled: true,
                    fillColor: PiccoloTheme.porcelain,
                    contentPadding: EdgeInsets.symmetric(
                      horizontal: Spacing.base,
                      vertical: Spacing.md,
                    ),
                  ),
                  onSubmitted: (_) => _searchImages(),
                ),
              ),
              const SizedBox(width: Spacing.md),
              FilledButton(
                onPressed: _isSearching ? null : _searchImages,
                child: _isSearching
                    ? const SizedBox(
                        width: 20,
                        height: 20,
                        child: CircularProgressIndicator(
                          strokeWidth: 2,
                          color: Colors.white,
                        ),
                      )
                    : const Text('Search'),
              ),
            ],
          ),
          if (_searchResults.isNotEmpty) ...[
            const SizedBox(height: Spacing.base),
            ..._searchResults.take(10).map(_buildSearchResultTile),
          ],
          if (_error != null)
            Container(
              margin: const EdgeInsets.only(top: Spacing.base),
              padding: const EdgeInsets.all(Spacing.md),
              decoration: BoxDecoration(
                color: PiccoloTheme.critical.withValues(alpha: 0.1),
                borderRadius: BorderRadius.circular(Radii.sm),
              ),
              child: Row(
                children: [
                  const Icon(PiccoloIcons.error, color: PiccoloTheme.critical),
                  const SizedBox(width: Spacing.md),
                  Expanded(
                    child: Text(
                      _error!,
                      style: const TextStyle(color: PiccoloTheme.critical),
                    ),
                  ),
                ],
              ),
            ),
          const SizedBox(height: Spacing.base),
          Container(
            padding: const EdgeInsets.all(Spacing.md),
            decoration: BoxDecoration(
              color: PiccoloTheme.warning.withValues(alpha: 0.1),
              borderRadius: BorderRadius.circular(Radii.sm),
            ),
            child: const Row(
              children: [
                Icon(PiccoloIcons.info, color: PiccoloTheme.warning),
                SizedBox(width: Spacing.md),
                Expanded(
                  child: Text(
                    "Custom images start with no network listeners. You can add ports via 'Edit Listeners' after creation.",
                    style: TextStyle(fontSize: 13),
                  ),
                ),
              ],
            ),
          ),
        ],
      ),
    );
  }

  Widget _buildCatalogCard(CatalogItem item) {
    return InkWell(
      onTap: () => _installFromCatalog(item),
      borderRadius: BorderRadius.circular(Radii.md),
      child: Container(
        width: 150,
        padding: const EdgeInsets.all(Spacing.base),
        decoration: BoxDecoration(
          color: PiccoloTheme.porcelain,
          borderRadius: BorderRadius.circular(Radii.md),
          border: Border.all(color: PiccoloTheme.hairline),
        ),
        child: Column(
          children: [
            AppIcon(
              proxyUrl: (item.icon ?? '').isNotEmpty
                  ? CoreConfig.catalogIconUrl(item.name)
                  : null,
              originalIconUrl: item.icon,
              size: 32,
              borderRadius: 8,
              fallbackIcon: PiccoloIcons.terminal,
              fallbackBackgroundColor: Colors.transparent,
            ),
            const SizedBox(height: Spacing.sm),
            Text(
              item.name.replaceFirst('workspace-', '').toUpperCase(),
              style: const TextStyle(fontWeight: FontWeight.bold),
              textAlign: TextAlign.center,
            ),
            Text(
              item.description,
              style: PiccoloTheme.textTheme.labelSmall,
              textAlign: TextAlign.center,
              maxLines: 2,
              overflow: TextOverflow.ellipsis,
            ),
          ],
        ),
      ),
    );
  }

  Widget _buildSearchResultTile(ImageSearchResult result) {
    return ListTile(
      leading: Icon(
        result.official ? PiccoloIcons.verified : PiccoloIcons.circuitry,
        color: result.official ? PiccoloTheme.success : PiccoloTheme.inkMuted,
      ),
      title: Text(result.name),
      subtitle: Text(
        result.description,
        maxLines: 1,
        overflow: TextOverflow.ellipsis,
      ),
      trailing: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          const Icon(PiccoloIcons.star, size: 16, color: PiccoloTheme.warning),
          const SizedBox(width: Spacing.xs),
          Text('${result.stars}'),
        ],
      ),
      onTap: () => _selectCustomImage(result.fullName, result.name),
    );
  }

  Widget _buildConfigureStep() {
    return SingleChildScrollView(
      padding: const EdgeInsets.all(Spacing.lg),
      child: Form(
        key: _formKey,
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
          // Selected image display
          Container(
            padding: const EdgeInsets.all(Spacing.base),
            decoration: BoxDecoration(
              color: PiccoloTheme.porcelain,
              borderRadius: BorderRadius.circular(Radii.md),
              border: Border.all(color: PiccoloTheme.hairline),
            ),
            child: Row(
              children: [
                const Icon(
                  PiccoloIcons.circuitry,
                  size: 32,
                  color: PiccoloTheme.cobalt600,
                ),
                const SizedBox(width: Spacing.base),
                Expanded(
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      const Text(
                        'Selected Image',
                        style: TextStyle(
                          color: PiccoloTheme.inkMuted,
                          fontSize: 12,
                        ),
                      ),
                      Text(
                        _selectedImageName ?? _selectedImage ?? '',
                        style: const TextStyle(fontWeight: FontWeight.bold),
                      ),
                    ],
                  ),
                ),
                TextButton(
                  onPressed: () => setState(() => _currentStep = 0),
                  child: const Text('Change'),
                ),
              ],
            ),
          ),
          const SizedBox(height: Spacing.lg),

          // Tag input
          Text(
            'Image Tag',
            style: PiccoloTheme.textTheme.bodyMedium?.copyWith(
              fontWeight: FontWeight.w600,
            ),
          ),
          const SizedBox(height: Spacing.sm),
          TextField(
            controller: _tagController,
            decoration: const InputDecoration(
              hintText: 'latest',
              border: OutlineInputBorder(),
              filled: true,
              fillColor: PiccoloTheme.porcelain,
              contentPadding: EdgeInsets.symmetric(
                horizontal: Spacing.base,
                vertical: Spacing.md,
              ),
            ),
          ),
          const SizedBox(height: Spacing.lg),

          // RFC 20260130: Workspace name is the identity for workspace mode apps
          Text(
            'Workspace Name',
            style: PiccoloTheme.textTheme.bodyMedium?.copyWith(
              fontWeight: FontWeight.w600,
            ),
          ),
          const SizedBox(height: Spacing.xs),
          Text(
            'Lowercase letters and numbers only, max 16 characters',
            style: PiccoloTheme.textTheme.labelSmall?.copyWith(
              color: PiccoloTheme.inkMuted,
            ),
          ),
          const SizedBox(height: Spacing.sm),
          TextFormField(
            controller: _workspaceNameController,
            autovalidateMode: AutovalidateMode.onUserInteraction,
            decoration: const InputDecoration(
              hintText: 'e.g., devserver',
              border: OutlineInputBorder(),
              filled: true,
              fillColor: PiccoloTheme.porcelain,
              contentPadding: EdgeInsets.symmetric(
                horizontal: Spacing.base,
                vertical: Spacing.md,
              ),
            ),
            validator: (value) {
              if (value == null || value.isEmpty) {
                return null; // Optional field, will use default from image name
              }
              // Must be lowercase letters and numbers only, start with letter, max 16 chars
              final regex = RegExp(r'^[a-z][a-z0-9]{0,15}$');
              if (!regex.hasMatch(value)) {
                if (value.length > 16) {
                  return 'Maximum 16 characters allowed';
                }
                if (!RegExp('^[a-z]').hasMatch(value)) {
                  return 'Must start with a lowercase letter';
                }
                return 'Only lowercase letters and numbers allowed';
              }
              return null;
            },
          ),
          const SizedBox(height: Spacing.base),

          // Info about ports
          Container(
            padding: const EdgeInsets.all(Spacing.md),
            decoration: BoxDecoration(
              color: PiccoloTheme.mist,
              borderRadius: BorderRadius.circular(Radii.sm),
            ),
            child: const Row(
              children: [
                Icon(PiccoloIcons.info, color: PiccoloTheme.inkMuted),
                SizedBox(width: Spacing.md),
                Expanded(
                  child: Text(
                    "This workspace will start with no network listeners. You can add ports via 'Edit Listeners' after creation.",
                    style: TextStyle(fontSize: 13),
                  ),
                ),
              ],
            ),
          ),

          if (_error != null)
            Container(
              margin: const EdgeInsets.only(top: Spacing.base),
              padding: const EdgeInsets.all(Spacing.md),
              decoration: BoxDecoration(
                color: PiccoloTheme.critical.withValues(alpha: 0.1),
                borderRadius: BorderRadius.circular(Radii.sm),
              ),
              child: Row(
                children: [
                  const Icon(PiccoloIcons.error, color: PiccoloTheme.critical),
                  const SizedBox(width: Spacing.md),
                  Expanded(
                    child: Text(
                      _error!,
                      style: const TextStyle(color: PiccoloTheme.critical),
                    ),
                  ),
                ],
              ),
            ),
          ],
        ),
      ),
    );
  }

  Widget _buildFooter() {
    return Container(
      padding: const EdgeInsets.all(Spacing.lg),
      decoration: const BoxDecoration(
        border: Border(top: BorderSide(color: PiccoloTheme.hairline)),
      ),
      child: Row(
        mainAxisAlignment: MainAxisAlignment.end,
        children: [
          if (_currentStep == 1)
            TextButton(
              onPressed: () => setState(() => _currentStep = 0),
              child: const Text('Back'),
            ),
          const SizedBox(width: Spacing.base),
          if (_currentStep < 2)
            FilledButton(
              onPressed: _currentStep == 0
                  ? null
                  : (_isInstalling ? null : _createCustomWorkspace),
              style: FilledButton.styleFrom(
                backgroundColor: PiccoloTheme.success,
              ),
              child: _isInstalling
                  ? const SizedBox(
                      width: 20,
                      height: 20,
                      child: CircularProgressIndicator(
                        strokeWidth: 2,
                        color: Colors.white,
                      ),
                    )
                  : const Text('Create Workspace'),
            ),
          if (_currentStep == 2)
            FilledButton(
              onPressed: () => Navigator.of(context).pop(),
              child: const Text('Done'),
            ),
        ],
      ),
    );
  }
}
