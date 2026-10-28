import { act, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { describe, expect, it, vi } from 'vitest';
import {
  Badge,
  Button,
  Dialog,
  EmptyState,
  Frame,
  Gauge,
  Input,
  Kbd,
  List,
  Readout,
  Select,
  Sheet,
  Skeleton,
  Table,
  Tabs,
  ToastViewport,
  toast,
  useToasts,
  type Column,
} from '.';

describe('Frame', () => {
  it('labels its region with the title readout', () => {
    render(
      <Frame title="Nodes" index="06" meta={<span>3 online</span>}>
        body
      </Frame>,
    );
    const region = screen.getByRole('region', { name: 'Nodes' });
    expect(region).toHaveAttribute('data-state', 'idle');
    expect(within(region).getByText('06')).toBeInTheDocument();
    expect(within(region).getByText('3 online')).toBeInTheDocument();
    expect(within(region).getByText('body')).toBeInTheDocument();
  });
});

describe('Readout', () => {
  it('formats numbers and exposes the final value to assistive tech', () => {
    render(<Readout label="Tokens" value={12345} unit="tok" />);
    expect(screen.getByText('Tokens')).toBeInTheDocument();
    expect(screen.getByLabelText('12,345')).toBeInTheDocument();
    expect(screen.getByText('tok')).toBeInTheDocument();
  });

  it('shows a dash for an empty value', () => {
    render(<Readout label="Node" value="" />);
    expect(screen.getByText('—')).toBeInTheDocument();
  });
});

describe('Gauge', () => {
  it('is a meter and switches to the warning hue above the threshold', () => {
    const { rerender } = render(<Gauge label="Memory" value={2} max={8} />);
    const meter = screen.getByRole('meter', { name: 'Memory' });
    expect(meter).toHaveAttribute('aria-valuenow', '2');
    expect(meter).toHaveAttribute('aria-valuetext', '25%');
    rerender(<Gauge label="Memory" value={7.5} max={8} />);
    expect(screen.getByRole('meter').closest('[data-alert]')).not.toBeNull();
  });
});

interface Row {
  id: string;
  name: string;
}
const rows: Row[] = [
  { id: 'a', name: 'alpha' },
  { id: 'b', name: 'beta' },
  { id: 'c', name: 'gamma' },
];
const cols: Column<Row>[] = [
  { key: 'id', header: 'Id', cell: (r) => r.id, mono: true },
  { key: 'name', header: 'Name', cell: (r) => r.name },
];

describe('Table', () => {
  it('moves focus with the arrow keys and activates with Enter', async () => {
    const user = userEvent.setup();
    const onActivate = vi.fn();
    render(<Table aria-label="Things" columns={cols} rows={rows} rowKey={(r) => r.id} onActivate={onActivate} selectedKey="b" />);
    const grid = screen.getByRole('table', { name: 'Things' });
    const bodyRows = within(grid).getAllByRole('row').slice(1);
    expect(bodyRows.map((r) => r.tabIndex)).toEqual([0, -1, -1]);
    expect(bodyRows[1]).toHaveAttribute('aria-selected', 'true');
    await user.tab();
    expect(bodyRows[0]).toHaveFocus();
    await user.keyboard('{ArrowDown}');
    expect(bodyRows[1]).toHaveFocus();
    await user.keyboard('{End}');
    expect(bodyRows[2]).toHaveFocus();
    await user.keyboard('{Enter}');
    expect(onActivate).toHaveBeenCalledWith(rows[2]);
    await user.keyboard('{Home}');
    expect(bodyRows[0]).toHaveFocus();
  });

  it('renders the empty state when there are no rows', () => {
    render(<Table aria-label="Things" columns={cols} rows={[]} rowKey={(r) => r.id} empty={<EmptyState title="Nothing here" />} />);
    expect(screen.getByText('Nothing here')).toBeInTheDocument();
    expect(screen.queryByRole('table')).toBeNull();
  });
});

describe('List', () => {
  it('is a keyboard-navigable listbox', async () => {
    const user = userEvent.setup();
    const onActivate = vi.fn();
    render(<List aria-label="Names" items={rows} itemKey={(r) => r.id} renderItem={(r) => r.name} onActivate={onActivate} />);
    const opts = screen.getAllByRole('option');
    await user.tab();
    expect(opts[0]).toHaveFocus();
    await user.keyboard('{ArrowDown}{ }');
    expect(onActivate).toHaveBeenCalledWith(rows[1]);
  });
});

describe('Button', () => {
  it('is disabled and marked busy while running', async () => {
    const onClick = vi.fn();
    render(
      <Button busy onClick={onClick}>
        Approve
      </Button>,
    );
    const b = screen.getByRole('button', { name: 'Approve' });
    expect(b).toBeDisabled();
    expect(b).toHaveAttribute('aria-busy', 'true');
    await userEvent.click(b);
    expect(onClick).not.toHaveBeenCalled();
  });
});

describe('Input', () => {
  it('associates label, hint and error', () => {
    const { rerender } = render(<Input label="API key" hint="From mycel user add" />);
    const input = screen.getByLabelText('API key');
    expect(input).toHaveAccessibleDescription('From mycel user add');
    rerender(<Input label="API key" error="Not valid" />);
    expect(screen.getByLabelText('API key')).toHaveAttribute('aria-invalid', 'true');
    expect(screen.getByRole('alert')).toHaveTextContent('Not valid');
  });
});

describe('Select', () => {
  it('renders a labelled trigger with the selected label', () => {
    render(
      <Select
        label="Motion"
        value="reduced"
        onValueChange={() => undefined}
        options={[
          { value: 'system', label: 'System' },
          { value: 'reduced', label: 'Reduced' },
        ]}
      />,
    );
    const trigger = screen.getByRole('combobox', { name: 'Motion' });
    expect(trigger).toHaveTextContent('Reduced');
  });
});

describe('Dialog and Sheet', () => {
  it('opens a labelled modal dialog and closes on Escape', async () => {
    const user = userEvent.setup();
    function Harness() {
      const [open, setOpen] = useState(true);
      return (
        <Dialog
          open={open}
          onOpenChange={setOpen}
          title="Freeze the network?"
          description="Everything stops."
          footer={<Button>Freeze</Button>}
        />
      );
    }
    render(<Harness />);
    const dlg = await screen.findByRole('dialog', { name: 'Freeze the network?' });
    expect(dlg).toHaveAccessibleDescription('Everything stops.');
    await user.keyboard('{Escape}');
    await vi.waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  });

  it('renders a sheet with a close button', async () => {
    render(
      <Sheet open onOpenChange={() => undefined} title="Views">
        content
      </Sheet>,
    );
    expect(await screen.findByRole('dialog', { name: 'Views' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Close' })).toBeInTheDocument();
  });
});

describe('Toast', () => {
  it('announces in a live region and can be dismissed', async () => {
    render(<ToastViewport />);
    act(() => {
      toast({ title: 'Approved grt_1', timeout: 0 });
    });
    const region = screen.getByRole('status');
    expect(region).toHaveAttribute('aria-live', 'polite');
    expect(within(region).getByText('Approved grt_1')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(useToasts.getState().items).toHaveLength(0);
  });
});

describe('Badge, Tabs, Kbd, EmptyState, Skeleton', () => {
  it('Badge carries its tone and pulse', () => {
    render(
      <Badge tone="alert" pulse>
        Frozen
      </Badge>,
    );
    const b = screen.getByText('Frozen').parentElement!;
    expect(b).toHaveAttribute('data-tone', 'alert');
    expect(b).toHaveAttribute('data-pulse', 'true');
  });

  it('Tabs switch panels with the keyboard', async () => {
    const user = userEvent.setup();
    render(
      <Tabs
        aria-label="Sections"
        items={[
          { value: 'a', label: 'Logs', panel: <p>log panel</p> },
          { value: 'b', label: 'Files', panel: <p>file panel</p>, count: 3 },
        ]}
      />,
    );
    expect(screen.getByRole('tablist', { name: 'Sections' })).toBeInTheDocument();
    expect(screen.getByText('log panel')).toBeVisible();
    await user.click(screen.getByRole('tab', { name: /Files/ }));
    expect(screen.getByRole('tab', { name: /Files/ })).toHaveAttribute('aria-selected', 'true');
    expect(screen.getByText('file panel')).toBeVisible();
  });

  it('Kbd renders the platform modifier', () => {
    render(<Kbd keys={['mod', 'K']} />);
    expect(screen.getByText('K').tagName).toBe('KBD');
    expect(screen.getByText(/^(⌘|Ctrl)$/)).toBeInTheDocument();
  });

  it('EmptyState and Skeleton render their content', () => {
    render(
      <>
        <EmptyState title="No grants" action={<Button>Request</Button>}>
          Nothing is waiting.
        </EmptyState>
        <Skeleton rows={2} label="Loading nodes" />
      </>,
    );
    expect(screen.getByText('No grants')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Request' })).toBeInTheDocument();
    expect(screen.getByRole('status', { name: 'Loading nodes' })).toBeInTheDocument();
  });
});
