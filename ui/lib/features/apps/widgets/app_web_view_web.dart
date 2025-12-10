import 'dart:async';
import 'dart:ui_web' as ui;
import 'package:flutter/material.dart';
import 'package:flutter/rendering.dart';
import 'package:web/web.dart' as web;
import '../../../shells/desktop/widgets/window_activity.dart';

class AppWebView extends StatefulWidget {
  final String url;
  
  const AppWebView({super.key, required this.url});

  @override
  State<AppWebView> createState() => _AppWebViewState();
}

class _AppWebViewState extends State<AppWebView> {
  late String viewType;
  static int _viewIdCounter = 0;
  static final Map<String, web.HTMLIFrameElement> _iframeRegistry = {};
  
  Timer? _monitorTimer;
  final GlobalKey _key = GlobalKey();

  @override
  void initState() {
    super.initState();
    viewType = 'iframe-view-${widget.url}-${_viewIdCounter++}';
    
    // Register the IFrame view factory
    // Note: In a real app, registration should happen once per viewType, 
    // but here viewType is unique per instance.
    ui.platformViewRegistry.registerViewFactory(
      viewType,
      (int viewId) {
        final iframe = web.HTMLIFrameElement();
        iframe.src = widget.url;
        iframe.style.border = 'none';
        iframe.style.width = '100%';
        iframe.style.height = '100%';
        
        // Default to inert until validated
        iframe.style.pointerEvents = 'none';
        iframe.setAttribute('inert', 'true');
        
        _iframeRegistry[viewType] = iframe;
        return iframe;
      },
    );

    // Start visibility monitoring
    _monitorTimer = Timer.periodic(const Duration(milliseconds: 300), (_) => _checkVisibility());
  }

  @override
  void dispose() {
    _monitorTimer?.cancel();
    _iframeRegistry.remove(viewType);
    super.dispose();
  }

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    _checkVisibility();
  }

  void _checkVisibility() {
    if (!mounted) return;

    final iframe = _iframeRegistry[viewType];
    if (iframe == null) return;

    // 1. Check Window Activity (Window Logic)
    final isWindowActive = WindowActivity.isWindowActive(context);

    // 2. Check Route Currency (Modal/Dialog Logic)
    final route = ModalRoute.of(context);
    final isRouteCurrent = route?.isCurrent ?? true;

    // 3. Hit Test Probe (Overlap/Z-Index Logic)
    bool isTopLevel = false;
    
    // We only probe if other checks pass, to save resources
    if (isWindowActive && isRouteCurrent) {
        final renderObject = _key.currentContext?.findRenderObject();
        if (renderObject is RenderBox && renderObject.hasSize) {
           final position = renderObject.localToGlobal(renderObject.size.center(Offset.zero));
           
           final result = BoxHitTestResult();
           // Hit test from the root of the render tree
           RendererBinding.instance.renderViews.first.hitTest(result, position: position);
           
           // Check if we are the first (top-most) relevant hit
           for (final entry in result.path) {
             if (entry.target == renderObject) {
               isTopLevel = true;
               break; 
             }
             // If we hit a Barrier or another opaque widget first, we are covered.
             // Note: WindowFrame wrappers or Listeners might appear before us. 
             // Strictly checking "== renderObject" is safest for "am I top?".
             // But PointerInterceptor or other transparent widgets might be in the path.
             // If we assume standard opaque blocking widgets (Modals), this works.
           }
        }
    }

    final shouldBeActive = isWindowActive && isRouteCurrent && isTopLevel;
    _updateIFrameState(iframe, shouldBeActive);
  }

  void _updateIFrameState(web.HTMLIFrameElement iframe, bool active) {
     if (active) {
       if (iframe.style.pointerEvents != 'auto') {
         iframe.style.pointerEvents = 'auto';
         iframe.removeAttribute('inert');
       }
     } else {
       if (iframe.style.pointerEvents != 'none') {
         iframe.style.pointerEvents = 'none';
         iframe.setAttribute('inert', 'true');
       }
     }
  }

  @override
  Widget build(BuildContext context) {
    return SizedBox(
      key: _key,
      width: double.infinity,
      height: double.infinity,
      child: HtmlElementView(viewType: viewType),
    );
  }
}
