import i18n from 'i18next'
import { initReactI18next } from 'react-i18next'

void i18n.use(initReactI18next).init({
  lng: 'en',
  fallbackLng: 'en',
  interpolation: { escapeValue: false },
  resources: {
    en: { translation: {
      product: 'PolyGate', subtitle: 'Container Cloud',
      overview: 'Overview', newApp: 'New app', source: 'Source ZIP',
      title: 'Your workspace', intro: 'Manage your applications and resource budget in one place.',
      localMode: 'Local development mode',
      localDetail: 'Apps are saved as drafts. Build and cluster deployment are not connected yet.',
      apps: 'Applications', emptyApps: 'No apps yet',
      quota: 'Configured resources', capacity: 'Your account limits',
      appName: 'App name', image: 'Proposed image reference', port: 'App port',
      cpuRequest: 'CPU request (m)', cpuLimit: 'CPU limit (m)', memory: 'Memory (MiB)',
      replicas: 'Replicas', persistent: 'Use persistent storage',
      createDraft: 'Save draft', cancel: 'Cancel',
      newAppTitle: 'Create an app draft', newAppIntro: 'Save the port and resource settings, then choose an approved image when you deploy.',
      zipTitle: 'Validate a source ZIP', zipIntro: 'Build submission is currently enabled for selected pilot accounts. This form checks the ZIP structure only.',
      chooseZIP: 'Choose a ZIP file', validateZIP: 'Validate ZIP',
      zipReady: 'ZIP passed validation. Build submission is not enabled for this account.',
      created: 'App draft saved', status: 'Status', createdAt: 'Created',
      limitMemory: 'Memory limit',
    } },
  },
})

export default i18n
