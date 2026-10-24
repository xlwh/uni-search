import { createRouter, createWebHistory } from 'vue-router'

const routes = [
  { path: '/', name: 'dashboard', component: () => import('@/views/DashboardView.vue'), meta: { title: '集群总览', icon: 'Odometer' } },
  { path: '/indices', name: 'indices', component: () => import('@/views/IndexManagementView.vue'), meta: { title: '索引管理', icon: 'Files' } },
  { path: '/search', name: 'search', component: () => import('@/views/SearchConsoleView.vue'), meta: { title: '搜索控制台', icon: 'Search' } },
  { path: '/documents', name: 'documents', component: () => import('@/views/DocumentManagementView.vue'), meta: { title: '文档管理', icon: 'Document' } },
  { path: '/tenants', name: 'tenants', component: () => import('@/views/TenantManagementView.vue'), meta: { title: '多租户管理', icon: 'User' } },
  { path: '/snapshots', name: 'snapshots', component: () => import('@/views/SnapshotView.vue'), meta: { title: '快照与备份', icon: 'Camera' } },
  { path: '/monitoring', name: 'monitoring', component: () => import('@/views/MonitoringView.vue'), meta: { title: '监控指标', icon: 'TrendCharts' } },
  { path: '/synonyms-pipeline', name: 'synonyms', component: () => import('@/views/SynonymsPipelineView.vue'), meta: { title: '同义词与管道', icon: 'Connection' } },
]

const router = createRouter({
  history: createWebHistory(),
  routes,
})

export default router
