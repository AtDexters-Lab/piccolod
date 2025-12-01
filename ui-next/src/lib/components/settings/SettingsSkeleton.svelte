<script lang="ts">
  // Reusable skeleton for settings sections
</script>

<div class="card-stack animate-pulse">
  <div class="grid gap-6 md:grid-cols-2">
    <!-- Card 1 -->
    <div class="card">
      <div class="h-4 w-1/3 bg-slate-200 dark:bg-slate-700 rounded mb-2"></div>
      <div class="h-3 w-2/3 bg-slate-200 dark:bg-slate-700 rounded mb-4 opacity-70"></div>
      <div class="h-8 w-full bg-slate-200 dark:bg-slate-700 rounded-lg"></div>
    </div>

    <!-- Card 2 -->
    <div class="card">
      <div class="h-4 w-1/4 bg-slate-200 dark:bg-slate-700 rounded mb-2"></div>
      <div class="h-3 w-1/2 bg-slate-200 dark:bg-slate-700 rounded mb-4 opacity-70"></div>
      <div class="h-8 w-full bg-slate-200 dark:bg-slate-700 rounded-lg"></div>
    </div>
  </div>
</div>

<style>
  .card {
    padding: 20px;
    border-radius: 16px;
    border: 1px solid var(--card-border);
    background: rgba(var(--sys-surface-variant), 0.6);
  }
</style>
