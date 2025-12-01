<script lang="ts">
  export let variant: 'success' | 'warning' | 'critical' | 'info' | 'neutral' = 'neutral';

  const variants = {
    success: 'bg-success/10 text-success border-success/20',
    warning: 'bg-warning/10 text-warning border-warning/20',
    critical: 'bg-critical/10 text-critical border-critical/20',
    info: 'bg-info/10 text-info border-info/20',
    neutral: 'bg-surface-muted text-muted border-surface-variant'
  };
</script>

<span
  class="inline-flex items-center rounded-full border px-2.5 py-0.5 text-xs font-medium {variants[variant]}"
>
  <slot />
</span>
