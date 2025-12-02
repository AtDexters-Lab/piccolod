import 'package:flutter/material.dart';

enum SetupState {
  loading, // Checking status
  welcome, // First run intro
  credentials, // Set password
  recovery, // Recovery key
  finishing, // Finalizing setup
  unlock, // Already initialized, just need password
  complete, // Done, go to desktop
}

class SetupController extends ChangeNotifier {
  SetupState _state = SetupState.loading;
  SetupState get state => _state;

  // Mock Data
  final String _deviceName = "Piccolo Node";
  String get deviceName => _deviceName;
  
  // Recovery Key Mock
  final List<String> _recoveryWords = List.generate(24, (index) => "word${index + 1}");
  List<String> get recoveryWords => _recoveryWords;

  SetupController() {
    _checkStatus();
  }

  Future<void> _checkStatus() async {
    // Simulate API call to /crypto/status
    await Future.delayed(const Duration(milliseconds: 800));
    
    // MOCK: Assume first run for now
    _state = SetupState.welcome;
    notifyListeners();
  }

  void startSetup() {
    _state = SetupState.credentials;
    notifyListeners();
  }

  Future<bool> submitCredentials(String password) async {
    // Simulate API /crypto/setup with password
    await Future.delayed(const Duration(seconds: 1));
    
    // Success -> Go to recovery
    _state = SetupState.recovery;
    notifyListeners();
    return true;
  }

  void completeSetup() {
    _state = SetupState.complete;
    notifyListeners();
  }
}
