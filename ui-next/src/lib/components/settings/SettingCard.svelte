<script lang="ts">
  export let title: string;
  export let description: string | undefined = undefined;
  export let actions: boolean = false;
</script>

<section class="card relative" aria-label={title}>
  <div class="card-head">
    <div>
      <p class="label">{title}</p>
      {#if description}<p class="desc">{description}</p>{/if}
    </div>
    {#if actions}
      <div class="actions">
        <slot name="actions" />
      </div>
    {/if}
  </div>
  <div class="card-body">
    <slot />
  </div>
</section>

<style>
  .card {
    border: 1px solid var(--card-border);
    background: var(--card-bg);
    border-radius: 16px;
    padding: 18px;
    box-shadow: var(--shadow-soft);
  }

  .card-head {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 12px;
  }

  .label {
    font-weight: 600;
    color: rgb(var(--sys-ink));
    margin: 0;
  }

  .desc {
    margin: 4px 0 0;
    color: rgb(var(--sys-ink-muted));
    font-size: 13px;
  }

  .actions {
    display: flex;
    gap: 8px;
  }

  .card-body {
    margin-top: 12px;
  }
</style>
