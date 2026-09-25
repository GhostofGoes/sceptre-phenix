import {
  Autocomplete,
  Breadcrumb,
  Button,
  Checkbox,
  Collapse,
  ConfigProgrammatic,
  Dialog,
  Dropdown,
  Field,
  Icon,
  Input,
  Loading,
  Modal,
  Navbar,
  Notification,
  Numberinput,
  Progress,
  Radio,
  Select,
  Switch,
  Table,
  Tabs,
  Tag,
  Toast,
  Tooltip,
  Upload,
} from 'buefy';

// Register only the Buefy components the UI uses: app.use(Buefy) pulls every
// component into the entry chunk. Add a plugin here before using a new b-* tag
// (test/buefy.test.js checks this); heavy components used by one view
// (colorpicker, datetimepicker, slider) are registered locally in that view so
// they load with its chunk.
export const buefyPlugins = [
  Autocomplete,
  Breadcrumb,
  Button,
  Checkbox,
  Collapse,
  Dialog,
  Dropdown,
  Field,
  Icon,
  Input,
  Loading,
  Modal,
  Navbar,
  Notification,
  Numberinput,
  Progress,
  Radio,
  Select,
  Switch,
  Table,
  Tabs,
  Tag,
  Toast,
  Tooltip,
  Upload,
];

export function installBuefy(app) {
  ConfigProgrammatic.setOptions({
    defaultIconComponent: 'font-awesome-icon',
    defaultIconPack: 'fas',
    defaultProgrammaticPromise: true,
  });
  buefyPlugins.forEach((plugin) => app.use(plugin));
}
