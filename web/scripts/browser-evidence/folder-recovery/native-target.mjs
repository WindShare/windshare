const DATABASE = 'windshare-folder-evidence-target'
const STORE = 'authority'

async function database() {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(DATABASE, 1)
    request.onupgradeneeded = () => request.result.createObjectStore(STORE)
    request.onerror = () => reject(request.error)
    request.onsuccess = () => resolve(request.result)
  })
}

export async function loadNativeTarget() {
  const db = await database()
  try {
    return await new Promise((resolve, reject) => {
      const request = db.transaction(STORE).objectStore(STORE).get('target')
      request.onsuccess = () => resolve(request.result)
      request.onerror = () => reject(request.error)
    })
  } finally { db.close() }
}

export function installNativePicker() {
  const button = document.createElement('button')
  button.textContent = 'Choose isolated evidence target'
  document.body.append(button)
  globalThis.__folderNativeReady = new Promise((resolve, reject) => {
    button.onclick = async () => {
      try {
        const target = await window.showDirectoryPicker({ mode: 'readwrite', id: 'windshare-folder-evidence' })
        const db = await database()
        try {
          await new Promise((complete, fail) => {
            const transaction = db.transaction(STORE, 'readwrite')
            transaction.objectStore(STORE).put(target, 'target')
            transaction.oncomplete = complete
            transaction.onerror = () => fail(transaction.error)
            transaction.onabort = () => fail(transaction.error ?? new Error('Target authority transaction aborted'))
          })
        } finally { db.close() }
        resolve({ name: target.name, permission: await target.queryPermission({ mode: 'readwrite' }) })
      } catch (error) { reject(error) }
    }
  })
}
