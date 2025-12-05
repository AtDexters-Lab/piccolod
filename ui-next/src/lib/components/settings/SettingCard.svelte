<script lang="ts">
  export let title: string;
  export let description: string | undefined = undefined;
  export let actions: boolean = false;
</script>

<section
  class="relative rounded-2xl border border-surface-variant bg-surface p-6 shadow-soft"
  aria-label={title}
>
  <div class="flex items-start justify-between gap-3">
    <div>
      <p class="font-display font-semibold text-ink">{title}</p>
      {#if description}<p class="mt-1 text-sm text-muted">{description}</p>{/if}
    </div>
    {#if actions}
      <div class="flex gap-2">
        <slot name="actions" />
      </div>
    {/if}
  </div>
  <div class="mt-4">
    <slot />
  </div>
</section>
