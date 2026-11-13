import { createHugeIcon } from "@/lib/huge-icon";
import Activity01 from "@hugeicons/core-free-icons/Activity01Icon";
import AiMagic from "@hugeicons/core-free-icons/AiMagicIcon";
import ChartHistogram from "@hugeicons/core-free-icons/ChartHistogramIcon";
import ChartLineData01 from "@hugeicons/core-free-icons/ChartLineData01Icon";
import Cloud from "@hugeicons/core-free-icons/CloudIcon";
import Container from "@hugeicons/core-free-icons/ContainerIcon";
import Cube from "@hugeicons/core-free-icons/CubeIcon";
import DashboardSquare01 from "@hugeicons/core-free-icons/DashboardSquare01Icon";
import Database01 from "@hugeicons/core-free-icons/Database01Icon";
import Global from "@hugeicons/core-free-icons/GlobalIcon";
import Gpu from "@hugeicons/core-free-icons/GpuIcon";
import Key01 from "@hugeicons/core-free-icons/Key01Icon";
import Layers01 from "@hugeicons/core-free-icons/Layers01Icon";
import Logout03 from "@hugeicons/core-free-icons/Logout03Icon";
import Note01 from "@hugeicons/core-free-icons/Note01Icon";
import Notification01 from "@hugeicons/core-free-icons/Notification01Icon";
import Blueprint from "@hugeicons/core-free-icons/BlueprintIcon";
import Rocket01 from "@hugeicons/core-free-icons/Rocket01Icon";
import ServerStack01 from "@hugeicons/core-free-icons/ServerStack01Icon";
import Settings01 from "@hugeicons/core-free-icons/Settings01Icon";
import ShipWheel from "@hugeicons/core-free-icons/ShipWheelIcon";
import SidebarLeft01 from "@hugeicons/core-free-icons/SidebarLeft01Icon";
import TestTube01 from "@hugeicons/core-free-icons/TestTube01Icon";
import TimeSchedule from "@hugeicons/core-free-icons/TimeScheduleIcon";
import ConnectIcon from "@hugeicons/core-free-icons/ConnectIcon";
import Filter from "@hugeicons/core-free-icons/FilterIcon";
import Cancel01 from "@hugeicons/core-free-icons/Cancel01Icon";
import Refresh01 from "@hugeicons/core-free-icons/Refresh01Icon";
import ArrowDown01 from "@hugeicons/core-free-icons/ArrowDown01Icon";
import ArrowRight01 from "@hugeicons/core-free-icons/ArrowRight01Icon";
import User from "@hugeicons/core-free-icons/UserIcon";
import View from "@hugeicons/core-free-icons/ViewIcon";
import ViewOff from "@hugeicons/core-free-icons/ViewOffIcon";
import Add01 from "@hugeicons/core-free-icons/Add01Icon";
import Blocked from "@hugeicons/core-free-icons/BlockedIcon";
import CloudUpload from "@hugeicons/core-free-icons/CloudUploadIcon";
import Delete02 from "@hugeicons/core-free-icons/Delete02Icon";
import ComputerTerminal01 from "@hugeicons/core-free-icons/ComputerTerminal01Icon";
import PencilEdit01 from "@hugeicons/core-free-icons/PencilEdit01Icon";
import File02 from "@hugeicons/core-free-icons/File02Icon";
import Unlink01 from "@hugeicons/core-free-icons/Unlink01Icon";
import ImageAdd01 from "@hugeicons/core-free-icons/ImageAdd01Icon";
import WorkHistory from "@hugeicons/core-free-icons/WorkHistoryIcon";
import LinkSquare02 from "@hugeicons/core-free-icons/LinkSquare02Icon";
import MoreHorizontal from "@hugeicons/core-free-icons/MoreHorizontalIcon";
import Copy01 from "@hugeicons/core-free-icons/Copy01Icon";

/**
 * The console's own icons (side menu, top bar), drawn from the same
 * HugeIcons stroke set as Zeron's components so the shell and the
 * components look like one family. Sizes come from the slot (menu 16,
 * buttons per Zeron's control size); do not size them per call site.
 */
export const ConsoleIcon = {
  dashboard: createHugeIcon(DashboardSquare01),
  monitor: createHugeIcon(Activity01),
  scheduling: createHugeIcon(TimeSchedule),
  analytics: createHugeIcon(ChartHistogram),
  logs: createHugeIcon(Note01),
  deployments: createHugeIcon(Rocket01),
  batches: createHugeIcon(Layers01),
  playground: createHugeIcon(TestTube01),
  evaluations: createHugeIcon(ChartLineData01),
  models: createHugeIcon(Cube),
  inferenceTemplates: createHugeIcon(Blueprint),
  images: createHugeIcon(Container),
  nodes: createHugeIcon(ServerStack01),
  gpus: createHugeIcon(Gpu),
  storage: createHugeIcon(Database01),
  networks: createHugeIcon(ConnectIcon),
  platforms: createHugeIcon(Cloud),
  kubernetes: createHugeIcon(ShipWheel),
  apiKeys: createHugeIcon(Key01),
  settings: createHugeIcon(Settings01),
  notifications: createHugeIcon(Notification01),
  assistant: createHugeIcon(AiMagic),
  language: createHugeIcon(Global),
  logout: createHugeIcon(Logout03),
  sidebar: createHugeIcon(SidebarLeft01),
  filter: createHugeIcon(Filter),
  close: createHugeIcon(Cancel01),
  refresh: createHugeIcon(Refresh01),
  chevronDown: createHugeIcon(ArrowDown01),
  chevronRight: createHugeIcon(ArrowRight01),
  account: createHugeIcon(User),
  show: createHugeIcon(View),
  hide: createHugeIcon(ViewOff),
  add: createHugeIcon(Add01),
  delete: createHugeIcon(Delete02),
  block: createHugeIcon(Blocked),
  upload: createHugeIcon(CloudUpload),
  unlink: createHugeIcon(Unlink01),
  manifest: createHugeIcon(File02),
  edit: createHugeIcon(PencilEdit01),
  terminal: createHugeIcon(ComputerTerminal01),
  imageAdd: createHugeIcon(ImageAdd01),
  history: createHugeIcon(WorkHistory),
  external: createHugeIcon(LinkSquare02),
  more: createHugeIcon(MoreHorizontal),
  copy: createHugeIcon(Copy01),
};
