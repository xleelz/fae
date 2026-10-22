// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/scratch.go#L2
typedef Chandef {
	chan sync = [0] of {bool};
	chan enq = [0] of {bool};
	chan deq = [0] of {bool,bool};
	chan sending = [0] of {bool};
	chan rcving = [0] of {bool};
	chan closing = [0] of {bool};
	int size = 0;
	int num_msgs = 0;
	bool closed = false;
}



init { 
	chan child_ScratchRemovalDropsBackupAck20 = [1] of {int}->
	run ScratchRemovalDropsBackupAck2(child_ScratchRemovalDropsBackupAck20)->
	child_ScratchRemovalDropsBackupAck20?0->
stop_process:skip
}

proctype ScratchRemovalDropsBackupAck2(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousScratchRemovalDropsBackupAck222 = [1] of {int}->
	chan child_AnonymousScratchRemovalDropsBackupAck221 = [1] of {int}->
	chan child_AnonymousScratchRemovalDropsBackupAck220 = [1] of {int}->
	Chandef removed_ch->
	Chandef backup_ch->
	Chandef stopped_ch->
	run sync_monitor(stopped_ch)->
	run sync_monitor(backup_ch)->
	run sync_monitor(removed_ch)->
	run AnonymousScratchRemovalDropsBackupAck22(backup_ch,removed_ch,stopped_ch,child_AnonymousScratchRemovalDropsBackupAck220)->
	run receiver(child_AnonymousScratchRemovalDropsBackupAck220)->
	run AnonymousScratchRemovalDropsBackupAck22(removed_ch,stopped_ch,backup_ch,child_AnonymousScratchRemovalDropsBackupAck221)->
	run receiver(child_AnonymousScratchRemovalDropsBackupAck221)->
	run AnonymousScratchRemovalDropsBackupAck22(stopped_ch,backup_ch,removed_ch,child_AnonymousScratchRemovalDropsBackupAck222)->
	run receiver(child_AnonymousScratchRemovalDropsBackupAck222)->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/scratch.go:2:189 */
	:: removed_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/scratch.go:2:189 */->
	:: removed_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/scratch.go:2:189 */ -> 
		removed_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousScratchRemovalDropsBackupAck22(Chandef backup_ch;Chandef removed_ch;Chandef stopped_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/scratch.go:2:118 */
	:: stopped_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/scratch.go:2:118 */
	:: stopped_ch.sync!false -> 
		stopped_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/scratch.go:2:118 */
	fi->
	stop_process: skip->
	child!0
}

 /* ================================================================================== */
 /* ================================================================================== */
 /* ================================================================================== */ 
proctype async_monitor(Chandef ch) {
do
:: true ->
if
:: ch.closed -> 
end: if
  :: ch.num_msgs > 0 -> // cannot send on closed channel
    end4: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.deq!true,ch.num_msgs -> 
  		 ch.num_msgs = ch.num_msgs - 1
    fi->
  :: else ->    end5: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.sync!true-> 
    fi->
  fi->
:: else ->
	if
	:: ch.num_msgs == ch.size ->
		end1: if
		  :: ch.deq!false,ch.num_msgs ->
		    ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		    ch.closed = true
		   :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	:: ch.num_msgs == 0 -> 
end2:		if
		:: ch.enq!true -> // a message has been received
			ch.num_msgs = ch.num_msgs + 1
		:: ch.closing!false -> // closing the channel
			ch.closed = true
		:: ch.rcving?false ->
 		    ch.sending!true->
		fi->
		:: else -> 
		end3: if
		  :: ch.enq!true->
		     ch.num_msgs = ch.num_msgs + 1
		  :: ch.deq!false,ch.num_msgs->
		     ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		     ch.closed = true
		  :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	fi->
fi->
od->
}

proctype sync_monitor(Chandef ch) {
do
:: true->
if
:: ch.closed ->
end: if
  :: ch.enq!false-> // cannot send on closed channel
  :: ch.closing!true -> // cannot close twice a channel
  :: ch.sending!false -> // sending state of channel (closed)
  :: ch.rcving?false->
  :: ch.sync!true-> // can always receive on a closed chan
  fi->
:: else -> 
end1: if
    :: ch.rcving?false ->
       ch.sending!true->
    :: ch.closing!false ->
      ch.closed = true
    fi->
fi->
od->
stop_process: skip
}

proctype receiver(chan c) {
c?0
}

