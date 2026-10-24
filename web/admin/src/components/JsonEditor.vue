<template>
  <div class="json-editor" ref="containerRef"></div>
</template>

<script setup lang="ts">
import { ref, onMounted, watch } from 'vue'
import { EditorState } from '@codemirror/state'
import { EditorView, lineNumbers } from '@codemirror/view'
import { json } from '@codemirror/lang-json'
import { oneDark } from '@codemirror/theme-one-dark'

const props = withDefaults(defineProps<{
  modelValue: string
  readOnly?: boolean
}>(), {
  readOnly: false,
})

const emit = defineEmits<{ 'update:modelValue': [value: string] }>()

const containerRef = ref<HTMLElement>()
let view: EditorView | null = null

onMounted(() => {
  if (!containerRef.value) return
  view = new EditorView({
    state: EditorState.create({
      doc: props.modelValue,
      extensions: [
        lineNumbers(),
        json(),
        oneDark,
        EditorView.editable.of(!props.readOnly),
        EditorView.updateListener.of((u) => {
          if (u.docChanged) {
            emit('update:modelValue', u.state.doc.toString())
          }
        }),
      ],
    }),
    parent: containerRef.value,
  })
})

watch(() => props.modelValue, (val) => {
  if (view && val !== view.state.doc.toString()) {
    view.dispatch({
      changes: { from: 0, to: view.state.doc.length, insert: val },
    })
  }
})
</script>

<style scoped>
.json-editor {
  border: 1px solid #dcdfe6;
  border-radius: 4px;
  overflow: hidden;
}
.json-editor :deep(.cm-editor) {
  font-size: 13px;
  max-height: 400px;
}
</style>
