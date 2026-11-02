<script>
  let { id = "", value = 0, min = 0, max = 10, defaultValue = 0, label = "", onchange } = $props();
  const clamp = (number) => Math.max(min, Math.min(max, number));
  const current = $derived(Number.isNaN(parseInt(value)) ? 0 : parseInt(value));
</script>

<div class="stepper-control">
  <button class="stepper-btn" type="button" data-action="decrease" aria-label={`Decrease ${label}`} aria-controls={id} disabled={current <= min} onclick={() => onchange(clamp(current - 1))}>−</button>
  <input type="number" id={id} value={value} min={min} max={max} data-default={defaultValue} oninput={(event) => onchange(event.target.value === "" ? "" : Number(event.target.value))} onblur={(event) => onchange(event.target.value === "" || Number.isNaN(parseInt(event.target.value)) ? defaultValue : clamp(parseInt(event.target.value)))} />
  <button class="stepper-btn" type="button" data-action="increase" aria-label={`Increase ${label}`} aria-controls={id} disabled={current >= max} onclick={() => onchange(clamp(current + 1))}>+</button>
</div>
