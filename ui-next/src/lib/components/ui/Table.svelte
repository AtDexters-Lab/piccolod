<script lang="ts">
  export let headers: string[] = [];
</script>

<div class="w-full overflow-x-auto rounded-lg border border-surface-variant">
  <table class="w-full text-left text-sm">
    <thead class="bg-surface-muted/50 text-ink font-semibold">
      <tr>
        {#each headers as header}
          <th class="px-6 py-4 whitespace-nowrap">{header}</th>
        {/each}
      </tr>
    </thead>
    <tbody class="divide-y divide-surface-variant bg-surface">
      <slot />
    </tbody>
  </table>
</div>
