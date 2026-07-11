import { useState } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import ConfigJsonMode from './ConfigJsonMode'
import { resetConfigSchemaCache } from './configSchema'
import { testRunConfigSchema } from './testSchemaFixture'

const validConfigJSON = JSON.stringify({
  sources: [{ zfsPath: { name: 'bulk-pool-01/archive@snap' } }],
  copies: 2,
  library: {
    changer: '/dev/sch0',
    drives: ['/dev/nst0', '/dev/nst1'],
    blankSlots: [1, 2],
    tapeCapacityBytes: 2500000000000,
  },
  redundancy: { targetPercentage: 10, sliceSizeBytes: 1073741824 },
  encryption: {
    recipients: ['age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4'],
    identity: 'AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREAL',
  },
  delivery: { webhookUrl: 'https://discord.com/api/webhooks/123/abc' },
})

function Wrapper({ initial }: { initial: string }) {
  const [text, setText] = useState(initial)

  return <ConfigJsonMode text={text} onTextChange={setText} />
}

function stubSchemaFetch() {
  vi.stubGlobal(
    'fetch',
    vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => testRunConfigSchema }),
  )
}

afterEach(() => {
  vi.unstubAllGlobals()
  resetConfigSchemaCache()
})

describe('ConfigJsonMode', () => {
  it('shows "no config yet" for an empty textarea', () => {
    stubSchemaFetch()
    render(<Wrapper initial="" />)

    expect(screen.getByText(/no config yet/i)).toBeInTheDocument()
  })

  it('shows a parse-error indicator for invalid JSON', () => {
    stubSchemaFetch()
    render(<Wrapper initial="not json" />)

    expect(screen.getByRole('alert')).toHaveTextContent(/invalid json/i)
  })

  it('shows a schema-invalid indicator once the schema loads, for well-formed but incomplete JSON', async () => {
    stubSchemaFetch()
    render(<Wrapper initial={JSON.stringify({ copies: 2 })} />)

    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent(/invalid/i)
    })
  })

  it('shows a valid indicator with source/copy counts for a schema-valid config', async () => {
    stubSchemaFetch()
    render(<Wrapper initial={validConfigJSON} />)

    await waitFor(() => {
      expect(screen.getByText(/valid · 1 source\(s\) · 2 copies/i)).toBeInTheDocument()
    })
  })

  it('loads a file selected via the upload control into the textarea', async () => {
    stubSchemaFetch()

    let currentText = ''
    const handleChange = vi.fn((text: string) => {
      currentText = text
    })

    render(<ConfigJsonMode text={currentText} onTextChange={handleChange} />)

    const file = new File([validConfigJSON], 'run-config.json', { type: 'application/json' })
    fireEvent.change(screen.getByLabelText('Upload run config file'), { target: { files: [file] } })

    await waitFor(() => {
      expect(handleChange).toHaveBeenCalledWith(validConfigJSON)
    })
  })
})
