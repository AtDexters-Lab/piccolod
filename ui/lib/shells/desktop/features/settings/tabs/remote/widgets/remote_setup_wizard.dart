import 'dart:async';
import 'package:flutter/material.dart';
import 'package:flutter/gestures.dart';
import 'package:url_launcher/url_launcher.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import '../remote_controller.dart';
import 'remote_preflight_list.dart';

class RemoteSetupWizard extends StatefulWidget {
  final RemoteController controller;

  const RemoteSetupWizard({super.key, required this.controller});

  @override
  State<RemoteSetupWizard> createState() => _RemoteSetupWizardState();
}

class _RemoteSetupWizardState extends State<RemoteSetupWizard> {
  // Step 0: Guide
  final TextEditingController _endpointCtrl = TextEditingController();
  final TextEditingController _tldCtrl = TextEditingController();
  final TextEditingController _portalCtrl = TextEditingController();
  final TextEditingController _secretCtrl = TextEditingController();

  // Step 2: Config
  String _selectedSolver = 'http-01';
  String? _selectedProvider;
  final Map<String, TextEditingController> _dnsFieldCtrls = {};

  @override
  void initState() {
    super.initState();
    // Pre-fill from status if available (e.g. resuming)
    final status = widget.controller.status;
    if (status != null) {
      if (status.endpoint != null) _endpointCtrl.text = status.endpoint!;
      if (status.tld != null) {
        _tldCtrl.text = status.tld!;
        // Default portal
        if (status.portalHostname != null) {
          _portalCtrl.text = status.portalHostname!;
        } else {
           _portalCtrl.text = "portal.${status.tld}";
        }
      }
      _selectedSolver = status.solver != 'unknown' ? status.solver : 'http-01';

      // Smart Resume Logic
      if (status.state == 'preflight_required') {
        widget.controller.wizardStep = 1;
        // Auto-run preflight if we are resuming into this state
        _autoRunPreflight();
      }
    }

    // Load guide info
    widget.controller.loadNexusGuide();
  }

  void _autoRunPreflight() {
    // 1-second delay for "heads up" then run
    Timer(const Duration(seconds: 1), () {
      if (mounted && !widget.controller.isRunningPreflight && widget.controller.preflightChecks.isEmpty) {
        widget.controller.runPreflight();
      }
    });
  }
  
  @override
  void dispose() {
    _endpointCtrl.dispose();
    _tldCtrl.dispose();
    _portalCtrl.dispose();
    _secretCtrl.dispose();
    for (var c in _dnsFieldCtrls.values) {
      c.dispose();
    }
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return Column(
      children: [
        _buildStepperHeader(),
        const SizedBox(height: 32),
        _buildCurrentStep(),
      ],
    );
  }

  Widget _buildStepperHeader() {
    return Row(
      children: [
        _buildStepIndicator(0, "Connect"),
        _buildStepSeparator(),
        _buildStepIndicator(1, "Preflight"),
        _buildStepSeparator(),
        _buildStepIndicator(2, "Configure"),
      ],
    );
  }

  Widget _buildStepIndicator(int step, String label) {
    final current = widget.controller.wizardStep;
    final isActive = step == current;
    final isCompleted = step < current;

    return Expanded(
      child: Column(
        children: [
          CircleAvatar(
            backgroundColor: isActive || isCompleted ? PiccoloTheme.cobalt600 : PiccoloTheme.mist,
            foregroundColor: isActive || isCompleted ? Colors.white : PiccoloTheme.inkMuted,
            child: isCompleted ? const Icon(Icons.check) : Text("${step + 1}"),
          ),
          const SizedBox(height: 8),
          Text(
            label,
            style: TextStyle(
              fontWeight: isActive ? FontWeight.bold : FontWeight.normal,
              color: isActive ? PiccoloTheme.ink : PiccoloTheme.inkMuted,
            ),
          ),
        ],
      ),
    );
  }

  Widget _buildStepSeparator() {
    return const SizedBox(width: 32, child: Divider());
  }

  Widget _buildCurrentStep() {
    switch (widget.controller.wizardStep) {
      case 0:
        return _buildStep0Guide();
      case 1:
        return _buildStep1Preflight();
      case 2:
        return _buildStep2Config();
      default:
        return const Text("Unknown step");
    }
  }

  // --- Step 0: Connect (Nexus Guide) ---

  Widget _buildStep0Guide() {
    final guide = widget.controller.guideInfo;
    if (guide == null) return const Center(child: CircularProgressIndicator());

    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text("Connect to Nexus", style: PiccoloTheme.textTheme.displayLarge?.copyWith(fontSize: 24)),
        const SizedBox(height: 16),
        const Text("To enable remote access, you need a Nexus Relay. You can host your own."),
        const SizedBox(height: 24),

        // Requirements
        if (guide.requirements.isNotEmpty) ...[
          Text("Prerequisites", style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
          const SizedBox(height: 8),
          ...guide.requirements.map((req) => Padding(
            padding: const EdgeInsets.only(bottom: 4),
            child: Row(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                const Icon(Icons.check_circle_outline, size: 16, color: PiccoloTheme.cobalt600),
                const SizedBox(width: 8),
                Expanded(child: Text(req)),
              ],
            ),
          )),
          const SizedBox(height: 24),
        ],

        // Command
        Text("Installation", style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
        const SizedBox(height: 8),
        Container(
          padding: const EdgeInsets.all(16),
          decoration: BoxDecoration(
            color: PiccoloTheme.ink,
            borderRadius: BorderRadius.circular(8),
          ),
          width: double.infinity,
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              SelectableText(
                guide.command,
                style: const TextStyle(fontFamily: 'monospace', color: PiccoloTheme.success, height: 1.5),
              ),
            ],
          ),
        ),
        Padding(
          padding: const EdgeInsets.only(top: 8),
          child: Text("Run this command on your VPS.", style: PiccoloTheme.textTheme.labelSmall),
        ),
        const SizedBox(height: 24),

        // Notes
        if (guide.notes.isNotEmpty) ...[
          Container(
            padding: const EdgeInsets.all(12),
            decoration: BoxDecoration(
              color: PiccoloTheme.info.withValues(alpha: 0.1),
              borderRadius: BorderRadius.circular(8),
              border: Border.all(color: PiccoloTheme.info.withValues(alpha: 0.3)),
            ),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: guide.notes.map((note) => Padding(
                padding: const EdgeInsets.only(bottom: 4),
                child: Row(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    const Icon(Icons.info_outline, size: 16, color: PiccoloTheme.info),
                    const SizedBox(width: 8),
                    Expanded(child: Text(note, style: const TextStyle(fontSize: 13))),
                  ],
                ),
              )).toList(),
            ),
          ),
          const SizedBox(height: 24),
        ],

        // Docs Link
        if (guide.docsUrl.isNotEmpty)
           Padding(
             padding: const EdgeInsets.only(bottom: 24),
             child: Row(
               children: [
                 const Icon(Icons.description_outlined, size: 16, color: PiccoloTheme.inkMuted),
                 const SizedBox(width: 8),
                 Expanded(
                   child: Text.rich(
                     TextSpan(
                       children: [
                         const TextSpan(
                           text: "Documentation: ",
                           style: TextStyle(color: PiccoloTheme.inkMuted, fontSize: 12),
                         ),
                         TextSpan(
                           text: guide.docsUrl,
                           style: const TextStyle(
                             color: PiccoloTheme.cobalt600,
                             decoration: TextDecoration.underline,
                             fontSize: 12,
                           ),
                           recognizer: TapGestureRecognizer()
                             ..onTap = () => launchUrl(Uri.parse(guide.docsUrl)),
                         ),
                       ],
                     ),
                   ),
                 ),
               ],
             ),
           ),

        const Divider(),
        const SizedBox(height: 32),
        
        Text("Enter Connection Details", style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
        const SizedBox(height: 16),
        _buildTextField("Nexus Endpoint", _endpointCtrl, hint: "wss://nexus.example.com"),
        const SizedBox(height: 16),
        _buildTextField("Base Domain (TLD)", _tldCtrl, hint: "home.example.com", onChanged: (val) {
          if (_portalCtrl.text.isEmpty || _portalCtrl.text.endsWith(val)) {
            // Auto-update portal suggestion
            // This is naive but helpful
          }
        }),
        const SizedBox(height: 16),
        _buildTextField("Portal Hostname", _portalCtrl, hint: "portal.home.example.com"),
        const SizedBox(height: 16),
        _buildTextField("Device Secret", _secretCtrl, obscureText: true),
        
        const SizedBox(height: 32),
        Align(
          alignment: Alignment.centerRight,
          child: ElevatedButton(
            onPressed: () async {
               if (_endpointCtrl.text.isEmpty || _tldCtrl.text.isEmpty || _secretCtrl.text.isEmpty || _portalCtrl.text.isEmpty) {
                 ScaffoldMessenger.of(context).showSnackBar(const SnackBar(content: Text("Please fill all fields")));
                 return;
               }
               await widget.controller.verifyNexusGuide(
                 _endpointCtrl.text,
                 _tldCtrl.text,
                 _portalCtrl.text,
                 _secretCtrl.text,
               );
               // Auto-run preflight after transition
               _autoRunPreflight();
            },
            child: const Text("Next: Run Preflight"),
          ),
        ),
      ],
    );
  }

  // --- Step 1: Preflight ---

  Widget _buildStep1Preflight() {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text("Preflight Checks", style: PiccoloTheme.textTheme.displayLarge?.copyWith(fontSize: 24)),
        const SizedBox(height: 16),
        const Text("Verifying your environment and connection settings."),
        const SizedBox(height: 24),
        
        if (!widget.controller.isRunningPreflight && widget.controller.preflightChecks.isEmpty)
           Center(
             child: ElevatedButton(
               onPressed: widget.controller.runPreflight,
               child: const Text("Run Checks"),
             ),
           )
        else ...[
           RemotePreflightList(checks: widget.controller.preflightChecks),
           if (!widget.controller.isRunningPreflight)
             Padding(
               padding: const EdgeInsets.only(top: 16),
               child: Center(
                 child: TextButton.icon(
                   onPressed: widget.controller.runPreflight,
                   icon: const Icon(Icons.refresh),
                   label: const Text("Re-run Checks"),
                 ),
               ),
             ),
        ],

        if (widget.controller.isRunningPreflight)
          const Padding(padding: EdgeInsets.only(top: 16), child: Center(child: CircularProgressIndicator())),

        const SizedBox(height: 32),
        if (widget.controller.preflightChecks.isNotEmpty && !widget.controller.isRunningPreflight)
          Row(
            mainAxisAlignment: MainAxisAlignment.spaceBetween,
            children: [
              TextButton(onPressed: () {
                setState(() => widget.controller.wizardStep = 0);
              }, child: const Text("Back")),
              
              ElevatedButton(
                onPressed: widget.controller.preflightChecks.any((c) => c.status == 'fail') 
                    ? null // Disable if failed
                    : () async {
                      // Fetch DNS providers before moving if needed
                      await widget.controller.fetchDNSProviders();
                      setState(() => widget.controller.wizardStep = 2);
                    },
                child: const Text("Next: Configure"),
              ),
            ],
          ),
      ],
    );
  }

  // --- Step 2: Configure ---

  Widget _buildStep2Config() {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text("Configuration", style: PiccoloTheme.textTheme.displayLarge?.copyWith(fontSize: 24)),
        const SizedBox(height: 16),
        
        DropdownButtonFormField<String>(
          decoration: const InputDecoration(labelText: "ACME Solver", border: OutlineInputBorder()),
          initialValue: _selectedSolver,
          items: const [
            DropdownMenuItem(value: 'http-01', child: Text("HTTP-01 (Requires Port 80)")),
            DropdownMenuItem(value: 'dns-01', child: Text("DNS-01 (Requires API Key)")),
          ],
          onChanged: (val) {
            setState(() {
              _selectedSolver = val!;
            });
          },
        ),
        
        if (_selectedSolver == 'dns-01') ...[
          const SizedBox(height: 16),
          DropdownButtonFormField<String>(
            decoration: const InputDecoration(labelText: "DNS Provider", border: OutlineInputBorder()),
            initialValue: _selectedProvider,
            items: widget.controller.dnsProviders.map((p) => DropdownMenuItem(value: p.id, child: Text(p.name))).toList(),
            onChanged: (val) {
              setState(() {
                _selectedProvider = val;
                // Reset fields
                _dnsFieldCtrls.clear();
              });
            },
          ),
          if (_selectedProvider != null) ...[
            const SizedBox(height: 16),
            ..._buildDNSFields(),
          ]
        ],

        const SizedBox(height: 32),
        if (widget.controller.isSubmittingConfig)
          const Center(child: CircularProgressIndicator())
        else
          Row(
            mainAxisAlignment: MainAxisAlignment.spaceBetween,
            children: [
               TextButton(onPressed: () {
                setState(() => widget.controller.wizardStep = 1);
              }, child: const Text("Back")),
              ElevatedButton(
                onPressed: _submitConfig,
                child: const Text("Finish & Enable"),
              ),
            ],
          ),
      ],
    );
  }

  List<Widget> _buildDNSFields() {
    final provider = widget.controller.dnsProviders.firstWhere((p) => p.id == _selectedProvider);
    return provider.fields.map((field) {
      // Ensure controller exists
      _dnsFieldCtrls.putIfAbsent(field.id, () => TextEditingController());
      
      return Padding(
        padding: const EdgeInsets.only(bottom: 16),
        child: TextField(
          controller: _dnsFieldCtrls[field.id],
          obscureText: field.secret,
          decoration: InputDecoration(
            labelText: field.label,
            hintText: field.placeholder,
            helperText: field.description,
            border: const OutlineInputBorder(),
          ),
        ),
      );
    }).toList();
  }

  Widget _buildTextField(String label, TextEditingController ctrl, {String? hint, bool obscureText = false, Function(String)? onChanged}) {
    return TextField(
      controller: ctrl,
      obscureText: obscureText,
      onChanged: onChanged,
      decoration: InputDecoration(
        labelText: label,
        hintText: hint,
        border: const OutlineInputBorder(),
      ),
    );
  }

  void _submitConfig() {
    final Map<String, dynamic> dnsCreds = {};
    if (_selectedSolver == 'dns-01') {
      if (_selectedProvider == null) {
         ScaffoldMessenger.of(context).showSnackBar(const SnackBar(content: Text("Select a DNS provider")));
         return;
      }
      for (var entry in _dnsFieldCtrls.entries) {
        dnsCreds[entry.key] = entry.value.text;
      }
    }

    widget.controller.submitConfiguration({
      'endpoint': _endpointCtrl.text,
      'device_secret': _secretCtrl.text,
      'tld': _tldCtrl.text,
      'portal_hostname': _portalCtrl.text,
      'solver': _selectedSolver,
      'dns_provider': _selectedProvider,
      'dns_credentials': dnsCreds,
    });
  }
}
