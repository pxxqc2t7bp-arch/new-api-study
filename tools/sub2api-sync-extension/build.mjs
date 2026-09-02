import { cp, mkdir, readFile, rm, writeFile } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = dirname(fileURLToPath(import.meta.url))
const dist = resolve(root, 'dist')

await rm(dist, { force: true, recursive: true })
await mkdir(resolve(dist, 'site-adapters'), { recursive: true })

for (const file of ['background', 'content', 'contracts', 'options']) {
  const source = await readFile(resolve(root, 'src', `${file}.ts`), 'utf8')
  await writeFile(resolve(dist, `${file}.js`), source)
}

for (const file of ['leyi', 'hualong', 'ebond']) {
  const source = await readFile(
    resolve(root, 'src', 'site-adapters', `${file}.ts`),
    'utf8'
  )
  await writeFile(resolve(dist, 'site-adapters', `${file}.js`), source)
}

await cp(resolve(root, 'manifest.json'), resolve(dist, 'manifest.json'))
await cp(resolve(root, 'src', 'options.html'), resolve(dist, 'options.html'))
